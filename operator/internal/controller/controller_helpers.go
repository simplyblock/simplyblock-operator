package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resolveRebalancerImage returns the simplyblock-rebalancer image configured on
// the StorageCluster matching clusterUUID. Falls back to defaultRebalancerImage
// when the cluster has no explicit image pinned. Both migration validation/release
// jobs and replication preconnect jobs use this — same binary, same image source.
func resolveRebalancerImage(ctx context.Context, c client.Client, namespace, clusterUUID string) (string, error) {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := c.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list StorageClusters: %w", err)
	}
	for _, cr := range clusters.Items {
		if cr.Status.UUID != clusterUUID {
			continue
		}
		vm := cr.Spec.VolumeMigrationSettings
		if vm != nil && vm.RebalancerImage != nil && *vm.RebalancerImage != "" {
			return *vm.RebalancerImage, nil
		}
		break
	}
	return defaultRebalancerImage, nil
}

// checkVolumeMigrationEnabled returns an error when the StorageCluster matching
// clusterUUID explicitly disables volume migration. Used by the migration
// controller to gate job creation; replication preconnect does not consult it.
func checkVolumeMigrationEnabled(ctx context.Context, c client.Client, namespace, clusterUUID string) error {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := c.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list StorageClusters: %w", err)
	}
	for _, cr := range clusters.Items {
		if cr.Status.UUID != clusterUUID {
			continue
		}
		vm := cr.Spec.VolumeMigrationSettings
		if vm != nil && vm.Enabled != nil && !*vm.Enabled {
			return fmt.Errorf("volume migration is disabled for cluster UUID %q", clusterUUID)
		}
		return nil
	}
	return fmt.Errorf("no StorageCluster found for cluster UUID %q", clusterUUID)
}

// findConsumerNode returns the Kubernetes hostname of the first Running pod
// that mounts a PVC backed by volumeID (the CSI volume UUID encoded in the
// PersistentVolume's volumeHandle). Returns "" when no active consumer exists.
// Uses an uncached reader to avoid stale cache decisions.
func findConsumerNode(ctx context.Context, reader client.Reader, volumeID string) (string, error) {
	var pvList corev1.PersistentVolumeList
	if err := reader.List(ctx, &pvList); err != nil {
		return "", fmt.Errorf("list PersistentVolumes: %w", err)
	}

	var pvcName, pvcNamespace string
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.ClaimRef == nil {
			continue
		}
		// VolumeHandle format: "<clusterID>:<poolID>:<lvolID>"
		parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
		if len(parts) != 3 || parts[2] != volumeID {
			continue
		}
		pvcName = pv.Spec.ClaimRef.Name
		pvcNamespace = pv.Spec.ClaimRef.Namespace
		break
	}
	if pvcName == "" {
		return "", nil
	}

	var podList corev1.PodList
	if err := reader.List(ctx, &podList, client.InNamespace(pvcNamespace)); err != nil {
		return "", fmt.Errorf("list pods in %s: %w", pvcNamespace, err)
	}

	var nodes []string
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
				nodes = append(nodes, pod.Spec.NodeName)
				break
			}
		}
	}
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return "", nil
	}
	return nodes[0], nil
}

// rebalancerJobParams holds the caller-specific knobs for buildRebalancerJob.
type rebalancerJobParams struct {
	Name          string
	Namespace     string
	OwnerRef      metav1.OwnerReference
	Hostname      string
	Image         string
	ContainerName string
	Mode          string
	Env           []corev1.EnvVar
	BackoffLimit  int32
	TTL           int32
	Deadline      int64
}

// buildRebalancerJob creates a node-pinned privileged Job running
// simplyblock-rebalancer. Migration validation/release and replication
// preconnect jobs share this builder to avoid duplicating the HostNetwork,
// volume-mount, and security-context boilerplate that every mode requires.
func buildRebalancerJob(p rebalancerJobParams) *batchv1.Job {
	privileged := true
	readOnly := true
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            p.Name,
			Namespace:       p.Namespace,
			OwnerReferences: []metav1.OwnerReference{p.OwnerRef},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &p.BackoffLimit,
			TTLSecondsAfterFinished: &p.TTL,
			ActiveDeadlineSeconds:   &p.Deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": p.Hostname},
					HostNetwork:   true,
					Volumes: []corev1.Volume{
						{
							Name: "host-dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/dev"},
							},
						},
						{
							// The subsystem presence check reads the host's NVMe sysfs
							// (/sys/class/nvme-subsystem); the container's own /sys is not.
							Name: "host-sys",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/sys"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            p.ContainerName,
							Image:           p.Image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"simplyblock-rebalancer", "--mode=" + p.Mode},
							Env:             p.Env,
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-dev", MountPath: "/dev"},
								{Name: "host-sys", MountPath: "/host/sys", ReadOnly: readOnly},
							},
						},
					},
				},
			},
		},
	}
}

// lvolConnRespToVmigConns maps webapi.LvolConnectResp entries to the
// vmigration.Connection type consumed by simplyblock-rebalancer Jobs.
func lvolConnRespToVmigConns(conns []webapi.LvolConnectResp) []vmigration.Connection {
	out := make([]vmigration.Connection, len(conns))
	for i, c := range conns {
		out[i] = vmigration.Connection{
			NQN:            c.Nqn,
			IP:             c.IP,
			Port:           c.Port,
			Transport:      c.TargetType,
			NrIoQueues:     c.NrIoQueues,
			ReconnectDelay: c.ReconnectDelay,
			// Use the canonical loss timeout rather than whatever the control plane
			// returns — see CtrlLossTmoSec for the rationale.
			CtrlLossTmo:   vmigration.CtrlLossTmoSec,
			FastIOFailTmo: c.FastIOFailTmo,
			KeepAliveTmo:  c.KeepAliveTmo,
		}
	}
	return out
}
