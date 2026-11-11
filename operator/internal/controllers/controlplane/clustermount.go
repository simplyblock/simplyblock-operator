// The one-time replacement of the cluster file's old subPath mount on a live
// Deployment. Server-side apply cannot remove a list entry another field manager
// still owns, and the Helm chart co-owns that mount on every Deployment it
// created before the operator adopted it.

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// applyManagementAPI replaces any old cluster-file mount, then applies the
// management API's objects.
func (r *ControlPlaneReconciler) applyManagementAPI(ctx context.Context, cp *simplyblockv1alpha2.ControlPlane) error {
	objects := managementAPIObjects(cp)
	for _, obj := range objects {
		d, ok := obj.(*appsv1.Deployment)
		if !ok {
			continue
		}
		if err := r.replaceSubPathClusterFileMounts(ctx, client.ObjectKeyFromObject(d)); err != nil {
			return err
		}
	}
	return applyAll(ctx, r.Client, cp, r.Scheme, objects)
}

// legacyClusterFileHashAnnotation is the stamp earlier releases rolled the
// Deployments with.
const legacyClusterFileHashAnnotation = "storage.simplyblock.io/cluster-file-hash"

// jsonPatchOp is one RFC 6902 operation.
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// replaceSubPathClusterFileMounts swaps each subPath mount of the cluster file on
// the live Deployment for the directory mount in one patch, so no pod is ever
// created without the file. Its test operations fail the patch if the object
// changed since it was read, and the next reconcile retries.
func (r *ControlPlaneReconciler) replaceSubPathClusterFileMounts(ctx context.Context, key client.ObjectKey) error {
	var live appsv1.Deployment
	if err := r.Get(ctx, key, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	var ops []jsonPatchOp
	spec := live.Spec.Template.Spec
	ops = append(ops, clusterFileMountOps("containers", spec.Containers)...)
	ops = append(ops, clusterFileMountOps("initContainers", spec.InitContainers)...)
	if len(ops) == 0 {
		return nil
	}
	// The stamp earlier releases put on the template goes in the same patch, so
	// the apply after it changes nothing and the Deployment rolls once.
	if _, stamped := live.Spec.Template.Annotations[legacyClusterFileHashAnnotation]; stamped {
		ops = append(ops, jsonPatchOp{Op: "remove", Path: "/spec/template/metadata/annotations/" +
			strings.ReplaceAll(legacyClusterFileHashAnnotation, "/", "~1")})
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	if err := r.Patch(ctx, &live, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		return fmt.Errorf("replace the subPath cluster-file mount of Deployment %s: %w", key.Name, err)
	}
	return nil
}

// clusterFileMountOps is the patch for one container list. The highest index
// goes first, so a removal never shifts an index still to be patched. The lowest
// becomes the directory mount unless the container already has it.
func clusterFileMountOps(field string, containers []corev1.Container) []jsonPatchOp {
	var ops []jsonPatchOp
	for ci, c := range containers {
		hasDir := false
		var legacy []int
		for mi, m := range c.VolumeMounts {
			switch {
			case m.Name != clusterFileVolume:
			case m.SubPath != "":
				legacy = append(legacy, mi)
			case m.MountPath == clusterFileDir:
				hasDir = true
			}
		}
		for i := len(legacy) - 1; i >= 0; i-- {
			mi := legacy[i]
			path := fmt.Sprintf("/spec/template/spec/%s/%d/volumeMounts/%d", field, ci, mi)
			ops = append(ops, jsonPatchOp{Op: "test", Path: path, Value: c.VolumeMounts[mi]})
			if i == 0 && !hasDir {
				ops = append(ops, jsonPatchOp{Op: "replace", Path: path, Value: corev1.VolumeMount{
					Name: clusterFileVolume, MountPath: clusterFileDir, ReadOnly: c.VolumeMounts[mi].ReadOnly,
				}})
				continue
			}
			ops = append(ops, jsonPatchOp{Op: "remove", Path: path})
		}
	}
	return ops
}
