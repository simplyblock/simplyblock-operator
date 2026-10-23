// Controller calls on a volume that has moved (design-superclusters.md §8.3).
//
// A PersistentVolume's handle is immutable, and after a move between members
// of a supercluster, or a fail-over, it names a source volume the control
// plane has deleted. The replication relationship survives the source, and
// following it reaches the volume serving the data. A call addressing the
// handle tries the handle first and follows the relationship only on a 404,
// so a volume that never moved costs nothing.
//
// A PV a Relocate moved carries storage.simplyblock.io/relocated-to, the
// handle of the volume the move put it on. Resolution starts there, which is
// what lets the source member go: once every PV naming it is marked, nothing
// reads its relationship records. DeleteVolume deletes the volume the mark
// names when it is still the active one; an unmarked handle never has its
// active volume deleted, because after a disaster-recovery fail-over a PV in
// another Kubernetes cluster carries the same handle and runs on it.

package controller

import (
	"context"
	"errors"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/lvol"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
)

// notFound is a control-plane call answered 404.
func notFound(err error) bool {
	var httpErr *controlplane.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound
}

// relocatedMark is the relocated-to mark of the PersistentVolume carrying
// volumeID, or an empty string when it carries none or cannot be read.
func (cs *Server) relocatedMark(ctx context.Context, volumeID string) string {
	if cs.kubeClient == nil {
		return ""
	}
	pvs, err := cs.kubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Warningf("reading the PersistentVolume of %s for a relocation mark: %v", volumeID, err)
		return ""
	}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeHandle == volumeID {
			return pv.Annotations[kube.AnnoRelocatedTo]
		}
	}
	return ""
}

// movedTo is the volume serving a handle whose own volume is gone: the end of
// its replication chain, walked from the PV's mark when it carries one. ok is
// false when the chain ends at the handle itself.
func (cs *Server) movedTo(ctx context.Context, volumeID string, h *lvol.Handle) (*lvol.Handle, bool, error) {
	start := *h
	if mark, ok := lvol.ParseHandle(lvol.VolumeHandle(cs.relocatedMark(ctx, volumeID))); ok {
		start = mark
	}
	client, err := clusters.ReplicationClient(ctx, start.ClusterID)
	if err != nil {
		return nil, false, err
	}
	hops, _, err := atlascp.WalkChain(ctx, start, client, clusters.ReplicationClient)
	if err != nil {
		return nil, false, err
	}
	end := hops[len(hops)-1].Handle
	if end == *h {
		return nil, false, nil
	}
	klog.Infof("volume %s has moved; addressing %s", volumeID, end.Handle())
	return &end, true, nil
}

// onActiveVolume runs call against the handle, and once more against the
// volume serving it when the handle's own volume is gone.
func (cs *Server) onActiveVolume(
	ctx context.Context, volumeID string, h *lvol.Handle, client *controlplane.ClusterClient,
	call func(*lvol.Handle, *controlplane.ClusterClient) error,
) error {
	err := call(h, client)
	if !notFound(err) {
		return err
	}
	moved, ok, merr := cs.movedTo(ctx, volumeID, h)
	if merr != nil {
		klog.Warningf("volume %s is gone and its relationship could not be followed: %v", volumeID, merr)
		return err
	}
	if !ok {
		return err
	}
	movedClient, cerr := clusters.Client(ctx, moved.ClusterID, moved.PoolRef)
	if cerr != nil {
		return cerr
	}
	return call(moved, movedClient)
}

// deleteRelocationTarget deletes the volume a PV's mark names, when it is
// still the active volume of its chain. A mark superseded by a later
// fail-over of the destination deletes nothing: the active volume is the one
// that fail-over made, and it belongs to whatever holds that.
func (cs *Server) deleteRelocationTarget(ctx context.Context, mark string) error {
	target, ok := lvol.ParseHandle(lvol.VolumeHandle(mark))
	if !ok {
		return nil
	}
	client, err := clusters.ReplicationClient(ctx, target.ClusterID)
	if errors.Is(err, controlplane.ErrClusterNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	hops, _, err := atlascp.WalkChain(ctx, target, client, clusters.ReplicationClient)
	if err != nil {
		return err
	}
	if len(hops) != 1 {
		klog.Infof("relocation target %s was superseded by %s; not deleting the active volume",
			mark, hops[len(hops)-1].Handle.Handle())
		return nil
	}
	err = cs.deleteVolume(ctx, mark)
	if errors.Is(err, controlplane.ErrVolumeNotFound) || errors.Is(err, controlplane.ErrClusterNotFound) {
		return nil
	}
	return err
}
