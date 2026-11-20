// The cluster-file stamp: what rolls the management API's pods when the
// FoundationDB operator moves the coordinators, in place of a third-party
// reloader. It lives beside the install because it is applied with the
// workloads that mount the file.

package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// clusterFileHashAnnotation carries the hash of the cluster file's data on the
// pod template, so a change to the file is a change to the template.
const clusterFileHashAnnotation = "storage.simplyblock.io/cluster-file-hash"

// managementAPIStamped is managementAPIObjects with the cluster-file hash on
// every workload that mounts the file. A file that does not exist yet stamps
// nothing: the FoundationDB operator writes it, and the next pass stamps it. A
// file that vanishes leaves the stamp the workload already carries, so its
// absence is not a template change.
func (r *ControlPlaneReconciler) managementAPIStamped(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) ([]client.Object, error) {
	objects := managementAPIObjects(cp)

	var file corev1.ConfigMap
	key := client.ObjectKey{Namespace: cp.Namespace, Name: clusterFileConfigMapName}
	if err := r.Get(ctx, key, &file); err != nil {
		if apierrors.IsNotFound(err) {
			return objects, r.keepLiveStamps(ctx, objects)
		}
		return nil, err
	}

	hash := hashConfigMapData(file.Data)
	for _, obj := range objects {
		if d, ok := obj.(*appsv1.Deployment); ok && mountsClusterFile(d) {
			stampPodTemplate(d, hash)
		}
	}
	return objects, nil
}

// keepLiveStamps copies the stamp each live workload carries onto its desired
// object, so applying while the file is absent leaves the template as it is.
func (r *ControlPlaneReconciler) keepLiveStamps(ctx context.Context, objects []client.Object) error {
	for _, obj := range objects {
		desired, ok := obj.(*appsv1.Deployment)
		if !ok || !mountsClusterFile(desired) {
			continue
		}
		var live appsv1.Deployment
		if err := r.Get(ctx, client.ObjectKeyFromObject(desired), &live); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if hash, stamped := live.Spec.Template.Annotations[clusterFileHashAnnotation]; stamped {
			stampPodTemplate(desired, hash)
		}
	}
	return nil
}

// mountsClusterFile reports whether the Deployment's pods mount the cluster file.
func mountsClusterFile(d *appsv1.Deployment) bool {
	return slices.ContainsFunc(d.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return v.ConfigMap != nil && v.ConfigMap.Name == clusterFileConfigMapName
	})
}

// stampPodTemplate sets the hash on a copy of the annotations, so a map shared
// between templates is never written through.
func stampPodTemplate(d *appsv1.Deployment, hash string) {
	annotations := make(map[string]string, len(d.Spec.Template.Annotations)+1)
	for k, v := range d.Spec.Template.Annotations {
		annotations[k] = v
	}
	annotations[clusterFileHashAnnotation] = hash
	d.Spec.Template.Annotations = annotations
}

// hashConfigMapData is a digest of the data that does not depend on map order.
func hashConfigMapData(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	sum := sha256.New()
	for _, k := range keys {
		// The separators keep {"a": "bc"} and {"ab": "c"} apart.
		sum.Write([]byte(k + "\x00" + data[k] + "\x00"))
	}
	return hex.EncodeToString(sum.Sum(nil))[:16]
}

// clusterFileRequests maps a change to the cluster-file ConfigMap to the
// ControlPlanes in its namespace.
func (r *ControlPlaneReconciler) clusterFileRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != clusterFileConfigMapName {
		return nil
	}
	var planes simplyblockv1alpha2.ControlPlaneList
	if err := r.List(ctx, &planes, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(planes.Items))
	for i := range planes.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&planes.Items[i])})
	}
	return requests
}
