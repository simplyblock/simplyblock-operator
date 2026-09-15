// Sorting a node's backend volumes into the four buckets a drain acts on, and
// choosing where the movable ones go.
//
// It is separate from the drain's steps because it is a pure question about state
// — which volumes are on this node and what Kubernetes knows about each — and the
// steps are what act on the answer. Every one of the drain's five steps asks it,
// and two of them only ask it.
//
// design-storagenode.md §8.1 is the specification.

package node

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/kube"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// The reason labels the blocked-volume gauge carries. They are lowercase because
// a metric label value is not an API enum.
const (
	blockedPinned    = "pinned"
	blockedUnmanaged = "unmanaged"
)

// volumeCensus is what one classification pass found on a node.
//
// The four buckets are disjoint and every volume the control plane reports on the
// node is in exactly one of them, which is what makes "the drain is done" a
// statement about the census rather than about a counter.
type volumeCensus struct {
	// Managed are the volumes a PersistentVolume accounts for and nothing pins,
	// paired with the name of that PersistentVolume, because the migration is
	// addressed by the Kubernetes object rather than by the backend volume.
	Managed []managedVolume

	// Pinned carries a claim with the selected-storage-node annotation. It blocks:
	// moving it would violate the pin, and the operator does not remove the
	// annotation on the user's behalf, because a pin is a placement decision
	// somebody made deliberately (§8.1).
	Pinned []string

	// System matched spec.remove.systemVolumeFilterRegex. It is skipped by the
	// migration and deleted during verification, because these are the
	// rebalancer's own per-node benchmark volumes and moving one to a peer would
	// produce a benchmark measuring the wrong node.
	System []systemVolume

	// Unmanaged has no PersistentVolume behind it. It blocks, and blocking is the
	// only safe answer: migrating it moves data nothing in Kubernetes is tracking,
	// and deleting it destroys data nothing in Kubernetes is tracking.
	Unmanaged []string

	// Incomplete says at least one volume could not be classified because its
	// claim could not be read. Those volumes are counted as unmanaged for safety,
	// and this flag is what stops the drain acting on a census it knows is a
	// transient false positive.
	Incomplete bool
}

// managedVolume is one movable volume and the PersistentVolume that accounts for
// it.
type managedVolume struct {
	VolumeUUID string
	PVName     string
}

// systemVolume is one benchmark volume, carried with its pool because deleting a
// volume is addressed by both.
type systemVolume struct {
	VolumeUUID string
	PoolUUID   string
	Name       string
}

// classify lists every volume on one node and sorts it.
//
// It walks the cluster's pools because the control plane offers no per-node
// volume list, and it drops volumes already being deleted: the backend's deletion
// is asynchronous, so one stays in the list briefly after its DELETE returned.
func (r *StorageNodeOpsReconciler) classify(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	clusterID, nodeID string,
) (volumeCensus, error) {
	filter, err := systemVolumeFilter(ops)
	if err != nil {
		return volumeCensus{}, err
	}

	pools, err := r.API.StoragePools(ctx, clusterID)
	if err != nil {
		return volumeCensus{}, fmt.Errorf("list the cluster's pools: %w", err)
	}

	byVolumeUUID, err := r.persistentVolumesByVolumeUUID(ctx)
	if err != nil {
		return volumeCensus{}, err
	}

	var census volumeCensus
	for _, pool := range pools {
		volumes, err := r.API.PoolVolumes(ctx, clusterID, pool.UUID)
		if err != nil {
			return volumeCensus{}, fmt.Errorf("list the volumes of pool %s: %w", pool.UUID, err)
		}
		for _, volume := range volumes {
			if volume.PrimaryNodeUUID != nodeID || volume.Status == volumeStatusInDeletion {
				continue
			}
			r.sortVolume(ctx, volume, pool.UUID, filter, byVolumeUUID, &census)
		}
	}
	return census, nil
}

// volumeStatusInDeletion is the control plane's own spelling for a volume whose
// delete has been accepted and not yet completed.
const volumeStatusInDeletion = "in_deletion"

// sortVolume places one volume in the census.
func (r *StorageNodeOpsReconciler) sortVolume(
	ctx context.Context,
	volume webapi.VolumeInfo,
	poolUUID string,
	filter *regexp.Regexp,
	byVolumeUUID map[string]*corev1.PersistentVolume,
	census *volumeCensus,
) {
	if filter.MatchString(volume.Name) {
		census.System = append(census.System, systemVolume{
			VolumeUUID: volume.UUID, PoolUUID: poolUUID, Name: volume.Name,
		})
		return
	}

	pv, accounted := byVolumeUUID[volume.UUID]
	if !accounted {
		census.Unmanaged = append(census.Unmanaged, volume.UUID)
		return
	}

	// A PersistentVolume with no claim cannot be pinned, because the annotation
	// lives on the claim. It is movable.
	if pv.Spec.ClaimRef == nil {
		census.Managed = append(census.Managed,
			managedVolume{VolumeUUID: volume.UUID, PVName: pv.Name})
		return
	}

	var claim corev1.PersistentVolumeClaim
	key := types.NamespacedName{
		Namespace: pv.Spec.ClaimRef.Namespace,
		Name:      pv.Spec.ClaimRef.Name,
	}
	if err := r.Get(ctx, key, &claim); err != nil {
		// Counted as unmanaged, which blocks, and recorded as incomplete, which
		// is what tells the caller this is a transient false positive rather than
		// a volume nothing accounts for. An API server that is briefly away must
		// not be read as permission to move a volume nobody could classify.
		logf.FromContext(ctx).V(1).Info("a volume's claim could not be read; the census is incomplete",
			"volume", volume.UUID, "claim", key.String(), "err", err.Error())
		census.Unmanaged = append(census.Unmanaged, volume.UUID)
		census.Incomplete = true
		return
	}

	if kube.IsPinnedVolume(claim.Annotations) {
		census.Pinned = append(census.Pinned, volume.UUID)
		return
	}
	census.Managed = append(census.Managed,
		managedVolume{VolumeUUID: volume.UUID, PVName: pv.Name})
}

// persistentVolumesByVolumeUUID indexes every simplyblock PersistentVolume in the
// cluster by the backend volume it names.
//
// The handle is clusterUUID:poolUUID:volumeUUID and the last segment is the
// volume, which is what makes the map key the same identity the control plane
// reports.
func (r *StorageNodeOpsReconciler) persistentVolumesByVolumeUUID(
	ctx context.Context,
) (map[string]*corev1.PersistentVolume, error) {
	var volumes corev1.PersistentVolumeList
	if err := r.List(ctx, &volumes); err != nil {
		return nil, fmt.Errorf("list persistent volumes: %w", err)
	}
	out := make(map[string]*corev1.PersistentVolume, len(volumes.Items))
	for i := range volumes.Items {
		pv := &volumes.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != utils.CSIProvisioner {
			continue
		}
		handle := pv.Spec.CSI.VolumeHandle
		if handle == "" {
			continue
		}
		parts := strings.SplitN(handle, ":", 3)
		if volumeUUID := parts[len(parts)-1]; volumeUUID != "" {
			out[volumeUUID] = pv
		}
	}
	return out, nil
}

// systemVolumeFilter compiles the operation's system-volume pattern.
//
// A pattern that does not compile is fatal rather than retried: the expression is
// in the spec and no number of passes will make it parse. The default is applied
// by the CRD, so an operation that states nothing arrives with the benchmark
// pattern already in place, and the fallback here covers an object written before
// the default existed.
func systemVolumeFilter(ops *simplyblockv1alpha2.StorageNodeOps) (*regexp.Regexp, error) {
	pattern := defaultSystemVolumePattern
	if stated := ops.Spec.RemoveParams().SystemVolumeFilterRegex; stated != nil && *stated != "" {
		pattern = *stated
	}
	filter, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fatalf("spec.remove.systemVolumeFilterRegex does not compile: %v", err)
	}
	return filter, nil
}

// defaultSystemVolumePattern matches the rebalancer's benchmark volumes by name,
// which is a convention rather than a guarantee: a volume a user happens to name
// this way is deleted during a drain's verification, and §16 Q1 is whether a label
// applied at creation should replace it.
const defaultSystemVolumePattern = `^sb-fio-baseline-.*`

// peerTargets assigns each movable volume an online peer to move to, round-robin.
//
// Round-robin spreads the drained node's volumes rather than concentrating them on
// whichever peer sorts first. The order is the peers' own UUIDs sorted, so the
// assignment is stable across passes: a volume that was assigned to one peer and
// whose migration then failed is reassigned by the caller deliberately rather than
// by the list having reshuffled.
//
// A drain with no online peer is a stall rather than a failure, which is why this
// reports a blockedStepError: the condition is resolved by another node coming
// back, and failing the operation would only mean starting it again afterward
// (§8.2).
func (r *StorageNodeOpsReconciler) peerTargets(
	ctx context.Context, clusterID, nodeID string, volumes []managedVolume,
) (map[string]string, error) {
	readings, err := r.clusterNodes(ctx, clusterID)
	if err != nil {
		return nil, err
	}

	peers := make([]string, 0, len(readings))
	for _, reading := range readings {
		if reading.UUID == nodeID || reading.Status != nodeStatusOnline {
			continue
		}
		peers = append(peers, reading.UUID)
	}
	if len(peers) == 0 {
		return nil, blockedf(NoMigrationTarget,
			"no online peer to move this node's volumes to; the drain resumes when one returns")
	}
	sort.Strings(peers)

	targets := make(map[string]string, len(volumes))
	for i, volume := range volumes {
		targets[volume.PVName] = peers[i%len(peers)]
	}
	return targets, nil
}

// clusterNodes returns every backend node of the cluster, from the stream's cache
// once it has delivered the cluster's snapshot and from the control plane until
// then.
//
// The gate is the snapshot rather than a preference: an empty unsynced cache and a
// cluster with no nodes look identical, and reading the first as the second would
// report a drain with no peers when every peer is there.
func (r *StorageNodeOpsReconciler) clusterNodes(
	ctx context.Context, clusterID string,
) ([]NodeReading, error) {
	if r.Nodes != nil && r.Nodes.Synced(scopeOf(clusterID)) {
		cached := r.Nodes.List(scopeOf(clusterID))
		out := make([]NodeReading, 0, len(cached))
		for _, dto := range cached {
			out = append(out, readingFromDTO(dto))
		}
		return out, nil
	}
	return r.API.StorageNodes(ctx, clusterID)
}

// scopeOf is the storage-node stream's scope for one cluster. Every subscription
// in this package is per cluster, so the scope is the cluster's UUID and nothing
// else.
func scopeOf(clusterID string) cpinformer.Scope { return cpinformer.Scope{clusterID} }
