// The NUMA device plugin the storage plane runs beside its storage nodes.
//
// The plugin advertises numa-align/numa-<socket> on every storage worker, and the
// control plane's storage pods request one so that a pod lands next to the
// memory and devices of a single socket. The OpenShift node bootstrap waits for
// the plugin's pod in kube-system by label, which is why it lives there and not
// in the operator's own namespace.
//
// That namespace decides how the object is owned. A StorageCluster cannot own an
// object in kube-system, because Kubernetes treats a namespaced owner in another
// namespace as one it cannot resolve and collects the object. The DaemonSet is
// also shared: it selects every enrolled worker whichever cluster enrolled it,
// so each cluster of the installation reconciles the same one. Ownership is
// therefore a label, and the last cluster to go removes it.

package node

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// NUMAPluginImageEnv names the image the plugin runs, so a disconnected
	// installation points it at a mirror.
	NUMAPluginImageEnv = "SB_NUMA_PLUGIN_IMAGE"

	// DefaultNUMAPluginImage is the release the chart pinned.
	DefaultNUMAPluginImage = "quay.io/simplyblock-io/numa-resource-plugin:latest"

	// numaPluginNamespace is where the OpenShift node bootstrap looks for the
	// plugin's pod. It is not configurable for the same reason.
	numaPluginNamespace = "kube-system"
	numaPluginName      = "simplyblock-numa-resource-plugin"

	// numaCapacity is what each NUMA node advertises. It is not a setting: the
	// node bootstrap waits for allocatable to read exactly this.
	numaCapacity = "100"

	numaManagedByLabel = "storage.simplyblock.io/managed-by"
	numaManagedByValue = "storagecluster-numa-plugin"
)

// numaPluginLabels select the plugin's pods, and are the labels the node
// bootstrap waits on.
var numaPluginLabels = map[string]string{"app": numaPluginName}

// numaPluginObjects are the plugin's account and DaemonSet.
func (r *StorageNodeWorkloadReconciler) numaPluginObjects() (*corev1.ServiceAccount, *appsv1.DaemonSet) {
	image := r.NUMAPluginImage
	if image == "" {
		image = DefaultNUMAPluginImage
	}
	meta := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name:      numaPluginName,
			Namespace: numaPluginNamespace,
			Labels:    map[string]string{"app": numaPluginName, numaManagedByLabel: numaManagedByValue},
		}
	}

	account := &corev1.ServiceAccount{ObjectMeta: meta()}
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: meta(),
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: numaPluginLabels},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      numaPluginLabels,
					Annotations: map[string]string{"log-collector/enabled": "true"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: numaPluginName,
					PriorityClassName:  "system-node-critical",
					// The storage plane: every worker a StorageCluster enrolled.
					Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{
								MatchExpressions: []corev1.NodeSelectorRequirement{{
									Key:      atlaskube.LabelStorageNodeSet,
									Operator: corev1.NodeSelectorOpExists,
								}},
							}},
						},
					}},
					// Every taint, control-plane nodes included: a storage worker
					// may be one.
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
					},
					Containers: []corev1.Container{{
						Name:            "numa-plugin",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						Env:             []corev1.EnvVar{{Name: "NUMA_CAPACITY", Value: numaCapacity}},
						SecurityContext: &corev1.SecurityContext{
							Privileged:               ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SELinuxOptions:           &corev1.SELinuxOptions{Type: "container_device_plugin_t"},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
								"/bin/sh", "-c", "ls /var/lib/kubelet/device-plugins/numa-*.sock >/dev/null 2>&1",
							}}},
							InitialDelaySeconds: 10,
							PeriodSeconds:       30,
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "device-plugin-kubelet", MountPath: "/var/lib/kubelet/device-plugins"},
							{Name: "sys", MountPath: "/sys", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "device-plugin-kubelet", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
							Path: "/var/lib/kubelet/device-plugins",
							Type: ptr.To(corev1.HostPathDirectoryOrCreate),
						}}},
						{Name: "sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
							Path: "/sys",
							Type: ptr.To(corev1.HostPathDirectory),
						}}},
					},
				},
			},
		},
	}
	return account, daemonSet
}

// reconcileNUMAPlugin applies the plugin, which every cluster of the installation
// does and which converges on the one DaemonSet.
//
// An object that already exists is updated with its own labels and annotations
// kept, so a DaemonSet the chart rendered keeps what Helm recorded on it.
func (r *StorageNodeWorkloadReconciler) reconcileNUMAPlugin(
	ctx context.Context, _ *simplyblockv1alpha2.StorageCluster,
) error {
	account, daemonSet := r.numaPluginObjects()
	if err := r.applyShared(ctx, account); err != nil {
		return fmt.Errorf("the NUMA plugin's account: %w", err)
	}
	if err := r.applyShared(ctx, daemonSet); err != nil {
		return fmt.Errorf("the NUMA plugin's daemon set: %w", err)
	}
	return nil
}

// applyShared creates the object or updates it in place. Metadata other writers
// recorded survives: only the keys this operator sets are overwritten.
func (r *StorageNodeWorkloadReconciler) applyShared(ctx context.Context, desired client.Object) error {
	existing := desired.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	desired.SetResourceVersion(existing.GetResourceVersion())
	desired.SetLabels(merged(existing.GetLabels(), desired.GetLabels()))
	desired.SetAnnotations(merged(existing.GetAnnotations(), desired.GetAnnotations()))
	// An account's token Secrets are the control plane's to manage, so an update
	// of the account stays out of them.
	if account, ok := desired.(*corev1.ServiceAccount); ok {
		current := existing.(*corev1.ServiceAccount)
		account.Secrets = current.Secrets
		account.ImagePullSecrets = current.ImagePullSecrets
	}
	return r.Update(ctx, desired)
}

func merged(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// releaseNUMAPlugin removes the plugin once no live StorageCluster remains. A
// cluster that is terminating is not live, so it does not keep the plugin alive
// through its own deletion.
//
// Only an object this operator marked is deleted. One by the same name that
// carries no mark is somebody else's.
func (r *StorageNodeWorkloadReconciler) releaseNUMAPlugin(ctx context.Context) error {
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return fmt.Errorf("list the storage clusters: %w", err)
	}
	for i := range clusters.Items {
		if clusters.Items[i].DeletionTimestamp.IsZero() {
			return nil
		}
	}

	account, daemonSet := r.numaPluginObjects()
	for _, obj := range []client.Object{daemonSet, account} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if obj.GetLabels()[numaManagedByLabel] != numaManagedByValue {
			continue
		}
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// clustersOfNUMAPlugin maps an event on the plugin's DaemonSet to every cluster,
// so that a DaemonSet somebody deleted is put back on the next pass rather than
// at the next unrelated event.
func (r *StorageNodeWorkloadReconciler) clustersOfNUMAPlugin(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	if obj.GetNamespace() != numaPluginNamespace || obj.GetName() != numaPluginName {
		return nil
	}
	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(clusters.Items))
	for i := range clusters.Items {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: clusters.Items[i].Namespace, Name: clusters.Items[i].Name,
		}})
	}
	return requests
}
