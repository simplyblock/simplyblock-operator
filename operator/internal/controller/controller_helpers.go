package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// requireStorageCluster returns an error when no StorageCluster in namespace
// reports clusterUUID. It is what stops the migration controller starting work
// against a cluster Kubernetes does not account for.
//
// It used to also refuse when volumeMigrationSettings.enabled was false. That
// field is gone (design-storagecluster.md §12): migration cannot be turned off,
// because a drain, a rebalance, and a device replacement are all performed by
// moving volumes, so a cluster that refused to move one could do none of them.
func requireStorageCluster(ctx context.Context, c client.Client, namespace, clusterUUID string) error {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := c.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list StorageClusters: %w", err)
	}
	for _, cr := range clusters.Items {
		if cr.Status.UUID == clusterUUID {
			return nil
		}
	}
	return fmt.Errorf("no StorageCluster found for cluster UUID %q", clusterUUID)
}

// findConsumerNode returns the Kubernetes hostname of the first Running pod
// that mounts a PVC backed by volumeID (the CSI volume UUID encoded in the
// PersistentVolume's volumeHandle). Returns "" when no active consumer exists.
// Uses an uncached reader to avoid stale cache decisions.
func findConsumerNode(ctx context.Context, reader client.Reader, volumeID string) (string, error) {
	var pvList corev1.PersistentVolumeList
	if err := reader.List(ctx, &pvList); err != nil {
		return "", fmt.Errorf("list PersistentVolumes: %w", err)
	}

	var pvcName, pvcNamespace string
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.ClaimRef == nil {
			continue
		}
		// VolumeHandle format: "<clusterID>:<poolID>:<lvolID>"
		parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
		if len(parts) != 3 || parts[2] != volumeID {
			continue
		}
		pvcName = pv.Spec.ClaimRef.Name
		pvcNamespace = pv.Spec.ClaimRef.Namespace
		break
	}
	if pvcName == "" {
		return "", nil
	}

	var podList corev1.PodList
	if err := reader.List(ctx, &podList, client.InNamespace(pvcNamespace)); err != nil {
		return "", fmt.Errorf("list pods in %s: %w", pvcNamespace, err)
	}

	var nodes []string
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
				nodes = append(nodes, pod.Spec.NodeName)
				break
			}
		}
	}
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return "", nil
	}
	return nodes[0], nil
}

// lvolConnRespToVmigConns maps webapi.LvolConnectResp entries to the
// vmigration.Connection type consumed by simplyblock-rebalancer Jobs.
func lvolConnRespToVmigConns(conns []webapi.LvolConnectResp) []vmigration.Connection {
	out := make([]vmigration.Connection, len(conns))
	for i, c := range conns {
		out[i] = vmigration.Connection{
			NQN:            c.Nqn,
			IP:             c.IP,
			Port:           c.Port,
			Transport:      c.TargetType,
			NrIoQueues:     c.NrIoQueues,
			ReconnectDelay: c.ReconnectDelay,
			// Use the canonical loss timeout rather than whatever the control plane
			// returns — see CtrlLossTmoSec for the rationale.
			CtrlLossTmo:   vmigration.CtrlLossTmoSec,
			FastIOFailTmo: c.FastIOFailTmo,
			KeepAliveTmo:  c.KeepAliveTmo,
		}
	}
	return out
}
