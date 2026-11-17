// Sealing a striped snapshot on its VolumeSnapshotContent, so that a retried
// CreateSnapshot can tell the snapshot an earlier attempt finished from one an
// attempt left behind.
//
// The control plane's snapshots carry no labels and cannot be renamed, so a
// complete set of member snapshots says nothing about the freeze it was taken
// under: an attempt whose freeze did not hold deletes its members, and a delete
// that fails leaves a complete set that is not one image. The seal is written
// after a freeze that held, on the VolumeSnapshotContent the snapshotter sidecar
// names in the request (--extra-create-metadata), and a retry answers a set
// only when the content carries its ID.

package controller

import (
	"context"
	"encoding/json"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/lvol"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

// paramSnapshotContentName is the parameter the snapshotter sidecar names the
// VolumeSnapshotContent in, with --extra-create-metadata.
const paramSnapshotContentName = "csi.storage.k8s.io/volumesnapshotcontent/name"

// snapshotSealer reads and writes the seal of a VolumeSnapshotContent.
type snapshotSealer interface {
	// Sealed is the snapshot ID the content is sealed with, empty for none.
	Sealed(ctx context.Context, content string) (string, error)
	// Seal records id on the content.
	Seal(ctx context.Context, content, id string) error
}

// annoStripedSnapshotSeal is the annotation a sealed VolumeSnapshotContent
// carries, valued with the striped snapshot's ID.
const annoStripedSnapshotSeal = "storage.simplyblock.io/striped-snapshot-id"

// volumeSnapshotContents is the resource the seal is written on.
var volumeSnapshotContents = schema.GroupVersionResource{
	Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotcontents",
}

// contentSealer seals VolumeSnapshotContents through the dynamic client, which
// the controller already holds and which needs none of the snapshotter's types.
type contentSealer struct {
	dyn dynamic.Interface
}

// NewContentSealer seals the VolumeSnapshotContents dyn reaches.
func NewContentSealer(dyn dynamic.Interface) snapshotSealer {
	return contentSealer{dyn: dyn}
}

// Sealed reads the content's seal.
func (s contentSealer) Sealed(ctx context.Context, content string) (string, error) {
	obj, err := s.dyn.Resource(volumeSnapshotContents).Get(ctx, content, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return obj.GetAnnotations()[annoStripedSnapshotSeal], nil
}

// Seal writes id on the content, as a merge patch of the one annotation, so it
// races nothing the snapshotter writes.
func (s contentSealer) Seal(ctx context.Context, content, id string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{annoStripedSnapshotSeal: id}},
	})
	if err != nil {
		return err
	}
	_, err = s.dyn.Resource(volumeSnapshotContents).Patch(ctx, content, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// SetSnapshotSealer gives the server the seals of VolumeSnapshotContents.
func (cs *Server) SetSnapshotSealer(s snapshotSealer) {
	cs.snapshotSeals = s
}

// sealedSnapshot answers a striped snapshot an earlier attempt finished: the
// one its VolumeSnapshotContent is sealed with, when every member snapshot is
// still there and of its member. Anything else is nil, and the snapshot is
// taken again, as an attempt that cannot read the seal also does.
func (cs *Server) sealedSnapshot(
	ctx context.Context, sbclient controlplane.ClusterAPI, req *csi.CreateSnapshotRequest,
	members []*controlplane.LvolResp, width int, sourceVolumeID string,
) *csi.CreateSnapshotResponse {
	content := req.GetParameters()[paramSnapshotContentName]
	if content == "" || cs.snapshotSeals == nil {
		return nil
	}
	id, err := cs.snapshotSeals.Sealed(ctx, content)
	if err != nil {
		klog.Warningf("striped snapshot %s: read the seal of %s, taking it again: %v", req.GetName(), content, err)
		return nil
	}
	base, sealedWidth, ok := lvol.SplitStripeMarker(id)
	if id == "" || !ok || sealedWidth != width {
		return nil
	}
	first, err := parseSnapshotID(base)
	if err != nil {
		return nil
	}
	snaps, err := sbclient.ListSnapshots(ctx)
	if err != nil {
		klog.Warningf("striped snapshot %s: list snapshots to answer its seal, taking it again: %v", req.GetName(), err)
		return nil
	}
	var largest int64
	for i, m := range members {
		found := false
		for _, s := range snaps {
			if s.Name == stripeMemberName(req.GetName(), i) && lvolIDFromURL(s.LvolURL) == m.UUID &&
				(i > 0 || s.UUID == first.snapshotID) {
				found = true
				break
			}
		}
		if !found {
			klog.Warningf("striped snapshot %s: sealed as %s and member %d is gone; taking it again",
				req.GetName(), id, i)
			return nil
		}
		largest = max(largest, m.LvolSize)
	}
	klog.Infof("striped snapshot %s: answering %s, sealed on %s by an earlier attempt", req.GetName(), id, content)
	return &csi.CreateSnapshotResponse{Snapshot: &csi.Snapshot{
		SizeBytes:      stripedRestoreBytes(largest, width),
		SnapshotId:     id,
		SourceVolumeId: sourceVolumeID,
		CreationTime:   timestamppb.Now(),
		ReadyToUse:     true,
	}}
}

// seal records a finished striped snapshot on its VolumeSnapshotContent. A seal
// that cannot be written costs only a second freeze on a retry, so it is
// logged and the snapshot still succeeds.
func (cs *Server) seal(ctx context.Context, req *csi.CreateSnapshotRequest, id string) {
	content := req.GetParameters()[paramSnapshotContentName]
	if content == "" || cs.snapshotSeals == nil {
		return
	}
	if err := cs.snapshotSeals.Seal(ctx, content, id); err != nil {
		klog.Warningf("striped snapshot %s: seal %s on %s: %v; a retry takes it again", req.GetName(), id, content, err)
	}
}
