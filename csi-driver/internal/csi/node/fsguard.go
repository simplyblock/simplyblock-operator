// The filesystem a volume is recorded as carrying, kept on its
// PersistentVolumeClaim.
//
// blkid reporting nothing means either that the device is blank or that it
// could not be read, and the two are indistinguishable from its exit code. The
// record settles it: a volume formatted once has an annotation, so a blank
// reading on it is a failed probe rather than an empty device, and staging
// mounts what the claim says instead of formatting over data.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"k8s.io/klog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/mount"
)

const (
	// annotationOnDiskFilesystem, set on a PersistentVolumeClaim, names the
	// filesystem to put on that claim's volume. It overrides the filesystem the
	// StorageClass asks for, which is what makes a single class usable by
	// workloads that disagree about the filesystem they want.
	annotationOnDiskFilesystem = "storage.simplyblock.io/on-disk-filesystem"

	// stagedFsTypeKey is the volume-context key under which the filesystem a
	// volume was staged with is recorded, alongside the other node-local keys
	// stashed at the staging path.
	stagedFsTypeKey = "stagedFsType"
)

// persistentVolumeClaimForVolume returns the PersistentVolumeClaim that owns the
// given CSI volume.
//
// The claim's namespace and name usually travel in the volume context: the
// external-provisioner runs with --extra-create-metadata, so CreateVolume was
// told which claim it was provisioning for and copied that into the context
// that became the PersistentVolume's volume attributes. A volume without them —
// provisioned by an older driver, or by a hand-written PersistentVolume — is
// resolved the long way instead: find the PersistentVolume carrying this volume
// handle, and follow its claim reference.
func (ns *Server) persistentVolumeClaimForVolume(
	ctx context.Context,
	volumeID string,
	volumeContext map[string]string,
) (*corev1.PersistentVolumeClaim, error) {
	namespace := strings.TrimSpace(volumeContext[csicommon.CSIStorageNamespaceKey])
	name := strings.TrimSpace(volumeContext[csicommon.CSIStorageNameKey])

	if namespace == "" || name == "" {
		spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
		if err != nil {
			return nil, fmt.Errorf("resolve claim for volume %s: %w", volumeID, err)
		}

		pv, err := ns.manager.PersistentVolumeByLogicalVolumeID(ctx, spdkVol.VolumeID)
		if err != nil {
			return nil, fmt.Errorf("resolve claim for volume %s: %w", volumeID, err)
		}
		if pv.Spec.ClaimRef == nil {
			return nil, fmt.Errorf(
				"resolve claim for volume %s: persistent volume %s is not bound to a claim",
				volumeID, pv.Name,
			)
		}
		namespace, name = pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
	}

	return ns.manager.PersistentVolumeClaimByNamespaceAndName(ctx, namespace, name)
}

// annotatedFilesystem returns the filesystem the volume's claim asks to have put
// on disk, and the empty string when the claim asks for none: that is the one
// reading under which staging carries on with the filesystem the volume
// capability names, exactly as it did before the annotation existed.
//
// It errors on the other two readings. A claim that cannot be read leaves the
// blank probe that led here unexplained, and a claim asking for a filesystem
// this driver does not create is an instruction that cannot be carried out;
// under either one, whether the device holds data is still open, so staging
// fails rather than formatting through the doubt.
func (ns *Server) annotatedFilesystem(
	ctx context.Context,
	volumeID string,
	volumeContext map[string]string,
) (string, error) {
	pvc, err := ns.persistentVolumeClaimForVolume(ctx, volumeID, volumeContext)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("volume %s: no claim to read %s from: %v", volumeID, annotationOnDiskFilesystem, err)
		} else {
			klog.Warningf("volume %s: failed to read %s from its claim: %v", volumeID, annotationOnDiskFilesystem, err)
		}
		return "", err
	}

	fsType := strings.ToLower(strings.TrimSpace(pvc.Annotations[annotationOnDiskFilesystem]))
	if fsType == "" {
		return "", nil
	}
	if !mount.Supported(fsType) {
		klog.Warningf(
			"claim %s/%s asks for on-disk filesystem %q, which this driver does not create; refusing to stage it",
			pvc.Namespace, pvc.Name, fsType,
		)
		return "", errors.New("unsupported filesystem type")
	}
	return fsType, nil
}

// recordOnDiskFilesystem writes the filesystem a volume was staged with onto its
// claim, under the same annotation that requests one. The annotation is a
// request only while the device is blank; from the first successful stage on it
// is the record of what is actually down there, which is why it is written back
// rather than left as whatever was asked for.
//
// It writes only when the claim does not already say this, so a volume that is
// staged on every pod start costs one write in total rather than one per start.
// Nothing here can fail staging: the volume is formatted and mounted by the time
// this runs, and a claim that cannot be read or written is a lost note, not a
// broken mount.
func (ns *Server) recordOnDiskFilesystem(
	ctx context.Context,
	volumeID string,
	volumeContext map[string]string,
	fsType string,
) {
	if ns.kubeClient == nil || fsType == "" {
		return
	}

	pvc, err := ns.persistentVolumeClaimForVolume(ctx, volumeID, volumeContext)
	if err != nil {
		klog.Warningf("volume %s: no claim to record the on-disk filesystem on: %v", volumeID, err)
		return
	}
	if pvc.Annotations[annotationOnDiskFilesystem] == fsType {
		return
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{annotationOnDiskFilesystem: fsType},
		},
	})
	if err != nil {
		klog.Warningf("volume %s: failed to build the %s patch: %v", volumeID, annotationOnDiskFilesystem, err)
		return
	}

	// A merge patch of the one key, so a concurrent writer of any other
	// annotation on this claim is left alone.
	if _, err := ns.kubeClient.CoreV1().PersistentVolumeClaims(pvc.Namespace).Patch(
		ctx, pvc.Name, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		klog.Warningf(
			"volume %s: failed to record %s=%s on claim %s/%s: %v",
			volumeID, annotationOnDiskFilesystem, fsType, pvc.Namespace, pvc.Name, err,
		)
		return
	}
	klog.Infof("volume %s: recorded %s=%s on claim %s/%s",
		volumeID, annotationOnDiskFilesystem, fsType, pvc.Namespace, pvc.Name)
}
