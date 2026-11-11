// The Jobs a HostMaintenance window creates, and how it reads them back.
//
// The window issues every FoundationDB command through a short-lived Job that
// runs fdbcli from the image the database's own pods run, with the cluster file
// mounted the way every other FoundationDB consumer mounts it. That keeps
// fdbcli at the server's version and keeps the operator's image and role as they
// are: no fdbcli in the manager, and no pods/exec in its role. The sentinel is
// a Job too, placed on the worker beside the held pods so that a drain, which
// evicts everything no budget holds, has something of the window's to evict.
//
// A Job is named for its operation and its command, so a step that finds its
// Job already present reads it rather than creating a second one, which is the
// write-ahead guard for every command. The one command whose output matters
// writes it to the container's termination message, which the pod's status
// carries, so no log access is needed.
//
// design-fdb-host-maintenance.md §5.3 and §5.4 are the specification.

package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// fdbCommand is one of the things the window asks FoundationDB, named as the
// suffix of the Job that asks it.
type fdbCommand string

const (
	fdbCommandMaintenanceOn     fdbCommand = "maintenance-on"
	fdbCommandCoordinators      fdbCommand = "coordinators"
	fdbCommandMaintenanceStatus fdbCommand = "maintenance-status"
	fdbCommandMaintenanceOff    fdbCommand = "maintenance-off"
	// fdbCommandSentinel is not a command: it is the pause pod of §5.3, named
	// with the same scheme so one function names every Job of a window.
	fdbCommandSentinel fdbCommand = "sentinel"
)

// The markers a window puts on what it creates and holds.
const (
	// fdbMaintenanceLabel marks the FoundationDB pods a window's budget selects.
	// Its value is the worker, so two windows on two workers each hold their
	// own pods. It is deliberately not the storage node's maintenance label, so
	// that one window releasing cannot strip the other's hold.
	fdbMaintenanceLabel = "storage.simplyblock.io/fdb-maintenance-worker"

	// fdbMaintenanceOpsLabel names the window a Job belongs to.
	fdbMaintenanceOpsLabel = "storage.simplyblock.io/controlplaneops"

	// fdbMaintenanceApp is the app label of the window's Jobs.
	fdbMaintenanceApp = "simplyblock-fdb-maintenance"

	// fdbcliPath is where the FoundationDB image carries the binary.
	fdbcliPath = "/usr/bin/fdbcli"

	// fdbcliTimeoutSeconds bounds one command against a database that cannot
	// answer, inside the Job's own deadline.
	fdbcliTimeoutSeconds = 30

	// commandJobAttempts, commandJobDeadlineSeconds, and jobTTLSeconds are the
	// Job's limits of design §5.4.
	commandJobAttempts        = 4
	commandJobDeadlineSeconds = 120
	jobTTLSeconds             = 3600
)

// fdbcliScript runs the command passed as the script's first argument, prints
// its output, and copies it to the termination message, which is what makes
// `maintenance status` readable from the pod's status. The exit code is the
// command's, which fdbcli --exec sets non-zero on failure.
var fdbcliScript = fmt.Sprintf(
	`out=$(%s -C %s --timeout %d --exec "$1" 2>&1); rc=$?; printf '%%s\n' "$out"; printf '%%s' "$out" > /dev/termination-log; exit $rc`,
	fdbcliPath, clusterFilePath, fdbcliTimeoutSeconds)

// fdbJobName is `<operation>-<command>`, shortened to a DNS label when the
// operation's name leaves no room, by hashing the part of it that is cut.
func fdbJobName(ops string, cmd fdbCommand) string {
	name := ops + "-" + string(cmd)
	if len(name) <= validation.DNS1123LabelMaxLength {
		return name
	}
	suffix := "-" + hash8(ops) + "-" + string(cmd)
	prefix := strings.TrimRight(ops[:validation.DNS1123LabelMaxLength-len(suffix)], "-")
	return prefix + suffix
}

// hash8 is the first eight hex digits of a SHA-256, which is the same
// construction the storage node's budget name uses.
func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// fdbCommandJob is the Job that runs one fdbcli command for the window. The
// Job lands where the database is allowed to run, tolerates a cordon so that it
// runs while the whole pool is cordoned, and stays off the window's own worker
// unless that worker is the only node there is.
func fdbCommandJob(
	ops *simplyblockv1alpha2.ControlPlaneOps,
	cp *simplyblockv1alpha2.ControlPlane,
	cmd fdbCommand,
	exec string,
	soleNode bool,
) *batchv1.Job {
	worker := ops.Spec.HostMaintenance.WorkerNode
	labels := windowJobLabels(ops)
	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		NodeSelector:  foundationDBNodeSelector(cp),
		Tolerations:   append(foundationDBTolerations(cp), cordonToleration()),
		Containers: []corev1.Container{{
			Name:            "fdbcli",
			Image:           image(fdbMonitorImage),
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"/bin/sh", "-c", fdbcliScript, "fdbcli"},
			Args:            []string{exec},
			VolumeMounts:    []corev1.VolumeMount{clusterFileMount()},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		}},
		Volumes: []corev1.Volume{clusterFileVolumeSource()},
	}
	if !soleNode {
		spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{
						Key:      "metadata.name",
						Operator: corev1.NodeSelectorOpNotIn,
						Values:   []string{worker},
					}},
				}},
			},
		}}
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fdbJobName(ops.Name, cmd),
			Namespace:       ops.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerOf(ops)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(commandJobAttempts - 1)),
			ActiveDeadlineSeconds:   ptr.To(int64(commandJobDeadlineSeconds)),
			TTLSecondsAfterFinished: ptr.To(int32(jobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{utils.AnnotationLogCollector: "true"},
				},
				Spec: spec,
			},
		},
	}
}

// sentinelJob is the pause pod of design §5.3, pinned to the worker with
// nodeName so that it is placed without the scheduler and so on a cordoned
// node. No budget selects it, and backoffLimit zero means nothing recreates it:
// its eviction is the drain made visible, once.
func sentinelJob(
	ops *simplyblockv1alpha2.ControlPlaneOps,
	cp *simplyblockv1alpha2.ControlPlane,
) *batchv1.Job {
	labels := windowJobLabels(ops)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fdbJobName(ops.Name, fdbCommandSentinel),
			Namespace:       ops.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerOf(ops)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			TTLSecondsAfterFinished: ptr.To(int32(jobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeName:      ops.Spec.HostMaintenance.WorkerNode,
					Tolerations:   append(foundationDBTolerations(cp), cordonToleration()),
					Containers: []corev1.Container{{
						Name:            "sentinel",
						Image:           image(fdbMonitorImage),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sleep", "infinity"},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("5m"),
								corev1.ResourceMemory: resource.MustParse("16Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
					}},
				},
			},
		},
	}
}

// windowJobLabels are the labels every Job of a window carries. The budget's
// label is deliberately not among them.
func windowJobLabels(ops *simplyblockv1alpha2.ControlPlaneOps) map[string]string {
	return map[string]string{
		appLabel:               fdbMaintenanceApp,
		fdbMaintenanceOpsLabel: ops.Name,
	}
}

// cordonToleration lets a pod be placed on a cordoned node, which is what a
// pool-wide cordon makes every node.
func cordonToleration() corev1.Toleration {
	return corev1.Toleration{
		Key:      corev1.TaintNodeUnschedulable,
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}
}

// ownerOf is the window as the controller of what it creates, so a deleted
// record takes its Jobs with it.
func ownerOf(ops *simplyblockv1alpha2.ControlPlaneOps) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         simplyblockv1alpha2.GroupVersion.String(),
		Kind:               "ControlPlaneOps",
		Name:               ops.Name,
		UID:                ops.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// ensureJob creates the desired Job when none by its name exists, and returns
// the one that exists otherwise. The name is the write-ahead guard: a pass that
// runs after a restart finds the Job the pass before it created.
func ensureJob(ctx context.Context, c client.Client, desired *batchv1.Job) (*batchv1.Job, error) {
	existing := &batchv1.Job{}
	key := client.ObjectKeyFromObject(desired)
	err := c.Get(ctx, key, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read Job %s: %w", key, err)
	}
	if err := c.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := c.Get(ctx, key, existing); err != nil {
				return nil, fmt.Errorf("read Job %s after losing the race to create it: %w", key, err)
			}
			return existing, nil
		}
		return nil, fmt.Errorf("create Job %s: %w", key, err)
	}
	return desired, nil
}

// errNoTerminationMessage is a Job none of whose pods has ended yet.
var errNoTerminationMessage = errors.New("the Job has no terminated pod with a message")

// jobTerminationMessage is the output of a command Job, read from the
// termination message its script wrote.
func jobTerminationMessage(ctx context.Context, c client.Reader, job *batchv1.Job) (string, error) {
	var pods corev1.PodList
	err := c.List(ctx, &pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name})
	if err != nil {
		return "", fmt.Errorf("list the pods of Job %s: %w", job.Name, err)
	}
	for i := range pods.Items {
		for _, status := range pods.Items[i].Status.ContainerStatuses {
			if status.State.Terminated != nil && status.State.Terminated.Message != "" {
				return status.State.Terminated.Message, nil
			}
		}
	}
	return "", errNoTerminationMessage
}
