package volumemigration

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// MigrationInitialDelay is the minimum time after calling CreateMigration before
	// polling the migration status. Prevents a race between the API call and the
	// control-plane migration tracker populating the record.
	MigrationInitialDelay = 20 * time.Second

	// MigrationStuckWarningTimeout is how long a migration can run before a
	// warning is logged.
	MigrationStuckWarningTimeout = 30 * time.Minute
)

// StartMigration creates a VolumeMigration CR for the given volume UUID and
// target node. It first resolves the volume UUID to a PV name by scanning all
// PersistentVolumes for a matching CSI volume handle.
// name is used as the VolumeMigration object name; namespace is the namespace
// to create it in; ownerRefs lets the caller attach owner references (e.g.
// a StorageCluster) so the CR is garbage-collected with the owner.
func StartMigration(
	ctx context.Context,
	c client.Client,
	volumeUUID, targetNodeUUID, name, namespace string,
	ownerRefs []metav1.OwnerReference,
) error {
	pvName, err := findPVForVolume(ctx, c, volumeUUID)
	if err != nil {
		return fmt.Errorf("resolve PV for volume %s: %w", volumeUUID, err)
	}
	vm := &simplyblockv1alpha1.VolumeMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: ownerRefs,
		},
		Spec: simplyblockv1alpha1.VolumeMigrationSpec{
			PVName:         pvName,
			TargetNodeUUID: targetNodeUUID,
		},
	}
	return c.Create(ctx, vm)
}

// findPVForVolume returns the PV name whose CSI volume handle equals volumeUUID.
func findPVForVolume(
	ctx context.Context,
	c client.Client,
	volumeUUID string,
) (string, error) {
	var pvList corev1.PersistentVolumeList
	if err := c.List(ctx, &pvList); err != nil {
		return "", fmt.Errorf("list PVs: %w", err)
	}
	for _, pv := range pvList.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeHandle == volumeUUID {
			return pv.Name, nil
		}
	}
	return "", fmt.Errorf("no PV found with CSI volume handle %q", volumeUUID)
}

// PollMigrationResult is returned by PollMigration.
type PollMigrationResult struct {
	// Done is true when CompletedAt > 0 in the migration record.
	Done bool
	// Succeeded is true when Done is true and no error message was set.
	Succeeded bool
	// Stuck is true when the migration has exceeded MigrationStuckWarningTimeout
	// without completing.
	Stuck bool
	// Migration is the raw DTO from the storage API (nil if still in initial delay).
	Migration *webapi.MigrationDTO
}

// PollMigration fetches the current status of an in-progress migration and
// returns a structured result. It respects the initial delay to avoid racing
// with the control-plane tracker. Both the VolumeRebalancer and VolumeMigration
// controllers call this to share identical polling semantics.
// Callers are responsible for acting on result.Stuck (logging, events, etc.)
// since each controller has its own event vocabulary.
func PollMigration(
	ctx context.Context,
	apiClient *webapi.Client,
	clusterUUID, poolUUID, volumeUUID, migrationID string,
	migrationStart time.Time,
) (PollMigrationResult, error) {
	if time.Now().Before(migrationStart.Add(MigrationInitialDelay)) {
		return PollMigrationResult{}, nil
	}

	m, err := apiClient.GetMigration(ctx, clusterUUID, poolUUID, volumeUUID, migrationID)
	if err != nil {
		return PollMigrationResult{}, err
	}

	result := PollMigrationResult{Migration: m}
	if m.CompletedAt > 0 {
		result.Done = true
		result.Succeeded = m.ErrorMessage == ""
		return result, nil
	}
	result.Stuck = time.Now().After(migrationStart.Add(MigrationStuckWarningTimeout))
	return result, nil
}


// nodesAboveThreshold returns the UUIDs of nodes whose latency deviation exceeds
// threshold, sorted by deviation descending (worst node first).
func NodesAboveThreshold(
	deviations map[string]float64,
	threshold float64,
) []string {
	type entry struct {
		uuid      string
		deviation float64
	}
	hot := make([]entry, 0, len(deviations))
	for uuid, dev := range deviations {
		if dev > threshold {
			hot = append(hot, entry{uuid, dev})
		}
	}
	sort.Slice(hot, func(i, j int) bool { return hot[i].deviation > hot[j].deviation })
	out := make([]string, len(hot))
	for i, e := range hot {
		out[i] = e.uuid
	}
	return out
}
