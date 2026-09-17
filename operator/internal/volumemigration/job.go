// The node-pinned Job every host-side migration mode runs in, and the image it
// runs.
//
// Both live here because both are asked by two controllers that must not answer
// differently. Validation, release, and replication preconnect are the same
// binary reading the same host state, so a Job that differed in its mounts or
// its privileges between them would be deciding about a fabric it cannot see.
// The image is the same question one step earlier: the binary in the Job has to
// be the one the cluster pinned.

package volumemigration

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// JobImageDefault is the image a Job runs when the cluster pins none.
//
// It is not DefaultRebalancerImage, and the two disagreeing is a fact about the
// code rather than a decision taken here: the settings block's default is the
// env-var-aware one the chart sets, and the Job's fallback has always been this
// literal. Moving the function did not change which image a Job runs.
const JobImageDefault = "docker.io/simplyblock/simplyblock-rebalancer:main"

// JobParams holds the caller-specific knobs for BuildJob.
type JobParams struct {
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

// BuildJob creates a node-pinned privileged Job running simplyblock-rebalancer.
//
// The owner reference is optional: a cluster-scoped operation cannot own a
// namespaced Job, so a caller that has no owner to name passes none and cleans
// its Jobs up itself.
func BuildJob(p JobParams) *batchv1.Job {
	privileged := true
	readOnly := true
	var owners []metav1.OwnerReference
	if p.OwnerRef.Name != "" {
		owners = []metav1.OwnerReference{p.OwnerRef}
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            p.Name,
			Namespace:       p.Namespace,
			OwnerReferences: owners,
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

// JobNameID renders a UUID into a DNS-label-safe fragment of a Job's name.
//
// The hyphens go and the result is capped, which is what keeps a Job name
// inside the 63 characters a label allows once a prefix and a node suffix are
// added to it. It is not reversible and does not need to be: what reads a Job
// name back is a human, and what finds a Job again is the record in status.
func JobNameID(uuid string) string {
	s := strings.ReplaceAll(uuid, "-", "")
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

// RecordVolumeMoved increments status.volumeMoveGeneration on the
// StorageCluster reporting clusterUUID, which records that one more volume has
// moved and a control-plane data realignment is owed.
//
// Both kinds of move call it, because the rebalancer's periodic loop reads the
// counter and compares it against status.realignedGeneration, and a move that
// did not count is a realignment that is never asked for.
//
// A counter rather than a flag, because both quantities matter: how many moves
// are outstanding, so a minimum can batch them, and whether a move landed after
// a realignment was already requested, so it is not silently absorbed by one
// that cannot account for it. The write retries on conflict, since two moves
// finishing at once would otherwise read the same value and one increment would
// vanish — and with batching, a lost increment delays a realignment
// indefinitely rather than by one cycle.
//
// Best effort in the caller's hands: a failure here is a realignment that is
// late rather than lost, because the next move to finish increments again.
func RecordVolumeMoved(
	ctx context.Context, c client.Client, namespace, clusterUUID string,
) (string, error) {
	if clusterUUID == "" {
		return "", nil
	}
	var name string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var clusters simplyblockv1alpha2.StorageClusterList
		if err := c.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
			return err
		}
		for i := range clusters.Items {
			cluster := &clusters.Items[i]
			if cluster.Status.UUID != clusterUUID {
				continue
			}
			name = cluster.Name
			patch := client.MergeFromWithOptions(
				cluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
			cluster.Status.VolumeMoveGeneration = ptr.To(
				ptr.Int64FromOrZero(cluster.Status.VolumeMoveGeneration) + 1)
			return c.Status().Patch(ctx, cluster, patch)
		}
		return nil
	})
	return name, err
}

// JobImage returns the simplyblock-rebalancer image configured on the
// StorageCluster reporting clusterUUID, falling back to JobImageDefault when
// the cluster pins none.
func JobImage(ctx context.Context, c client.Client, namespace, clusterUUID string) (string, error) {
	var clusters simplyblockv1alpha2.StorageClusterList
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
	return JobImageDefault, nil
}
