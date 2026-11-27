// The upgrade from the Prometheus StatefulSet a 26.3 install ran to the one the
// prometheus-operator runs: stop the old one, copy its volume into the claim the
// new one adopts by name, and only then apply the Prometheus resource. Each step
// is read from the cluster, so a pass interrupted at any point resumes.

package controlplane

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	prometheusCopyJob = ComponentPrometheus + "-copy"

	// copiedFromAnnotation on the new claim records a finished copy.
	copiedFromAnnotation = "storage.simplyblock.io/copied-from"

	// PrometheusCopyFailed is the event a failed copy emits. The new
	// Prometheus stays unapplied until the Job is deleted and the copy retried.
	PrometheusCopyFailed = "PrometheusCopyFailed"
)

// prometheusGated drops what cannot be applied yet: every prometheus-operator
// object when its kinds are not served, and the Prometheus resource until the
// old Prometheus's data is copied.
func (r *ControlPlaneReconciler) prometheusGated(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane, objects []client.Object,
) ([]client.Object, error) {
	if _, err := r.RESTMapper().RESTMapping(prometheusGVK.GroupKind(), prometheusGVK.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return without(objects, func(u *unstructured.Unstructured) bool {
				return u.GroupVersionKind().Group == prometheusGVK.Group
			}), nil
		}
		return nil, err
	}
	copied, err := r.prometheusDataCopied(ctx, cp)
	if err != nil || copied {
		return objects, err
	}
	return without(objects, func(u *unstructured.Unstructured) bool {
		return u.GroupVersionKind() == prometheusGVK
	}), nil
}

func without(objects []client.Object, drop func(*unstructured.Unstructured) bool) []client.Object {
	kept := objects[:0:0]
	for _, o := range objects {
		if u, ok := o.(*unstructured.Unstructured); ok && drop(u) {
			continue
		}
		kept = append(kept, o)
	}
	return kept
}

// prometheusDataCopied advances the copy by one step and reports whether it is
// done. An install without the old claim has nothing to copy.
func (r *ControlPlaneReconciler) prometheusDataCopied(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (bool, error) {
	ns := cp.Namespace
	var old corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: legacyPrometheusClaim}, &old); err != nil {
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}

	var claim corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: prometheusClaim}, &claim)
	switch {
	case apierrors.IsNotFound(err):
		claim = corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: prometheusClaim, Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: old.Spec.StorageClassName,
				Resources:        old.Spec.Resources,
			},
		}
		if err := r.Create(ctx, &claim); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	}
	if claim.Annotations[copiedFromAnnotation] != "" {
		return true, nil
	}

	if err := r.stopLegacyPrometheus(ctx, ns); err != nil {
		return false, err
	}
	err = r.Get(ctx, client.ObjectKey{Namespace: ns, Name: legacyPrometheusPod}, &corev1.Pod{})
	if err == nil || !apierrors.IsNotFound(err) {
		return false, client.IgnoreNotFound(err)
	}

	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Namespace: ns, Name: prometheusCopyJob}, &job)
	switch {
	case apierrors.IsNotFound(err):
		job := prometheusCopyJobObject(cp)
		if err := controllerutil.SetControllerReference(cp, job, r.Scheme); err != nil {
			return false, err
		}
		return false, r.Create(ctx, job)
	case err != nil:
		return false, err
	case job.Status.Succeeded > 0:
		patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, copiedFromAnnotation, legacyPrometheusClaim)
		if err := r.Patch(ctx, &claim, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			return false, err
		}
		return true, client.IgnoreNotFound(r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)))
	case job.Status.Failed > 0:
		r.emit(cp, corev1.EventTypeWarning, PrometheusCopyFailed, fmt.Sprintf(
			"copying %s into %s failed; see Job %s, and delete it to retry", legacyPrometheusClaim, prometheusClaim, prometheusCopyJob))
	}
	return false, nil
}

// removeObjectStore deletes the MinIO an earlier release ran for the Thanos
// sidecar's uploads, its claim included. Prometheus keeps its own history now.
func (r *ControlPlaneReconciler) removeObjectStore(ctx context.Context, ns string) error {
	named := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: ns} }
	for _, obj := range []client.Object{
		&appsv1.StatefulSet{ObjectMeta: named("simplyblock-minio")},
		&corev1.Service{ObjectMeta: named("simplyblock-minio")},
		&corev1.ConfigMap{ObjectMeta: named("simplyblock-objstore-config")},
		&corev1.PersistentVolumeClaim{ObjectMeta: named("minio-data-simplyblock-minio-0")},
	} {
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return err
		}
	}
	return nil
}

// stopLegacyPrometheus deletes the old StatefulSet, its configuration, and its
// headless Service. The StatefulSet's claim stays.
func (r *ControlPlaneReconciler) stopLegacyPrometheus(ctx context.Context, ns string) error {
	for _, obj := range []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: ComponentPrometheus, Namespace: ns}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyPrometheusConfig, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: legacyPrometheusHeadless, Namespace: ns}},
	} {
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return err
		}
	}
	return nil
}

// prometheusCopyJobObject copies the old volume into the subdirectory the
// prometheus-operator mounts as the data directory. It runs as the user both
// Prometheus run as, with the Prometheus image, which carries a shell.
func prometheusCopyJobObject(cp *simplyblockv1alpha2.ControlPlane) *batchv1.Job {
	local := cp.Spec.Source.Local
	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser: ptr.To(int64(65534)), RunAsGroup: ptr.To(int64(65534)),
			FSGroup: ptr.To(int64(65534)), RunAsNonRoot: ptr.To(true),
		},
		Containers: []corev1.Container{{
			Name:            "copy",
			Image:           image(prometheusImage),
			ImagePullPolicy: pullPolicyOf(local),
			Command: []string{"sh", "-ec",
				"mkdir -p /new/prometheus-db && cp -R /old/. /new/prometheus-db/ && rm -f /new/prometheus-db/lock"},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "old", MountPath: "/old", ReadOnly: true},
				{Name: "new", MountPath: "/new"},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "old", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: legacyPrometheusClaim, ReadOnly: true,
			}}},
			{Name: "new", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: prometheusClaim,
			}}},
		},
	}
	scheduling(local, &spec)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: prometheusCopyJob, Namespace: cp.Namespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(2)),
			Template:     corev1.PodTemplateSpec{Spec: spec},
		},
	}
}
