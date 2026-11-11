// The state volume of a storage cluster's metadata server: the 1 GiB
// simplyblock volume that holds the NFS client-recovery database, which the
// active guest connects itself (design-pnfs-mds-standby.md §5.4).
//
// It is created through the control plane rather than as a PVC so that no
// Kubernetes volume attach is involved: a pod stuck terminating on a dead node
// holds nothing a standby needs. The handle is recorded on the StatefulSet
// before any guest is promoted, and a finalizer there deletes the volume.

package mdsstate

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
)

// VolumeAPI is the part of the control-plane client the metadata server's
// volumes need. *controlplane.Client satisfies it.
type VolumeAPI interface {
	CreateVolume(ctx context.Context, clusterID, poolID string, p controlplane.CreateVolumeParams) (lvol.VolumeHandle, error)
	ListVolumes(ctx context.Context, clusterID, poolID string) ([]lvol.Volume, error)
	DeleteVolume(ctx context.Context, h lvol.VolumeHandle) error
	StoragePoolByName(ctx context.Context, clusterID, name string) (controlplane.StoragePool, error)
	EnsureVolumeHost(ctx context.Context, h lvol.VolumeHandle, hostNQN string) (controlplane.HostGrant, error)
	RemoveVolumeHost(ctx context.Context, h lvol.VolumeHandle, hostNQN string) error
}

// StateVolumeSize is 1 GiB. It is fixed because nfsdcld's database holds one
// row per client.
const StateVolumeSize = 1 << 30

// ErrStateUnavailable wraps every failure to create, find, or record the state
// volume. The caller turns it into an MDSStateUnavailable event and retries.
var ErrStateUnavailable = errors.New("the state volume is unavailable")

// StateVolume manages one storage cluster's state volume.
type StateVolume struct {
	Client client.Client
	API    VolumeAPI
}

// StateVolumeName is the volume's name in the control plane. It is derived from
// the StatefulSet's name so a volume created just before a crash is found again
// by name instead of created twice.
func StateVolumeName(statefulSetName string) string {
	return statefulSetName + "-state"
}

// Ensure returns the recorded handle. With none recorded, it adds the finalizer,
// looks the volume up by name in poolRef's pool, creates it when absent, and
// records its handle on the StatefulSet. poolRef is the pool segment of the
// first export's handle, an ID or a name.
func (s *StateVolume) Ensure(
	ctx context.Context, sts *appsv1.StatefulSet, clusterID, poolRef string,
) (lvol.VolumeHandle, error) {
	if recorded := sts.Annotations[StateVolumeAnnotation]; recorded != "" {
		return lvol.VolumeHandle(recorded), nil
	}
	unavailable := func(err error) (lvol.VolumeHandle, error) {
		return "", fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}

	// The finalizer goes first, so a StatefulSet deleted after the create is
	// still released only after the volume is dealt with.
	if !controllerutil.ContainsFinalizer(sts, StateVolumeFinalizer) {
		patch := client.MergeFromWithOptions(sts.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(sts, StateVolumeFinalizer)
		if err := s.Client.Patch(ctx, sts, patch); err != nil {
			return unavailable(fmt.Errorf("adding the finalizer to %s: %w", sts.Name, err))
		}
	}

	poolID, err := s.poolID(ctx, clusterID, poolRef)
	if err != nil {
		return unavailable(err)
	}
	name := StateVolumeName(sts.Name)
	existing, err := s.API.ListVolumes(ctx, clusterID, poolID)
	if err != nil {
		return unavailable(fmt.Errorf("looking for volume %s: %w", name, err))
	}
	var handle lvol.VolumeHandle
	for _, v := range existing {
		if v.Name == name {
			handle = v.ID
			break
		}
	}
	if handle == "" {
		handle, err = s.API.CreateVolume(ctx, clusterID, poolID,
			controlplane.CreateVolumeParams{Name: name, SizeBytes: StateVolumeSize})
		if err != nil {
			return unavailable(err)
		}
	}

	patch := client.MergeFrom(sts.DeepCopy())
	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	sts.Annotations[StateVolumeAnnotation] = string(handle)
	if err := s.Client.Patch(ctx, sts, patch); err != nil {
		return unavailable(fmt.Errorf("recording volume %s on %s: %w", handle, sts.Name, err))
	}
	return handle, nil
}

// poolID is the pool's ID, looked up by name when poolRef is a name.
func (s *StateVolume) poolID(ctx context.Context, clusterID, poolRef string) (string, error) {
	if lvol.IsCanonicalUUID(poolRef) {
		return poolRef, nil
	}
	pool, err := s.API.StoragePoolByName(ctx, clusterID, poolRef)
	if err != nil {
		return "", fmt.Errorf("resolving pool %q: %w", poolRef, err)
	}
	return pool.ID, nil
}

// Finalize deletes the recorded volume, counting one that is gone as deleted,
// and removes the finalizer. A failed delete keeps the finalizer and returns
// the error, so the volume is not orphaned.
func (s *StateVolume) Finalize(ctx context.Context, sts *appsv1.StatefulSet) error {
	if !controllerutil.ContainsFinalizer(sts, StateVolumeFinalizer) {
		return nil
	}
	if handle := sts.Annotations[StateVolumeAnnotation]; handle != "" {
		if err := s.API.DeleteVolume(ctx, lvol.VolumeHandle(handle)); err != nil {
			return fmt.Errorf("deleting the state volume %s: %w", handle, err)
		}
	}
	patch := client.MergeFromWithOptions(sts.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(sts, StateVolumeFinalizer)
	if err := s.Client.Patch(ctx, sts, patch); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("removing the finalizer from %s: %w", sts.Name, err)
	}
	return nil
}
