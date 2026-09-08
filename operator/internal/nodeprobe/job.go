// The Job that runs the probe on one worker.
//
// One Job per node, pinned by name. A DaemonSet would reach every worker
// without being told which, and it is the wrong shape twice: a probe is a
// one-shot that has to end, and a DaemonSet has no notion of a run that
// finished, so the discovery step would have nothing to wait on. A single Job
// with parallelism cannot be spread across specific nodes at all.
//
// The pod is privileged, and this is the file that owes a reason. The probe
// opens block devices, and a container's device cgroup denies that for every
// device the kubelet did not grant it, so an unprivileged probe can read sysfs
// and then not read a single disk — which is the reading that decides whether
// simplyblock would overwrite somebody's data. Everything it opens, it opens
// read-only: the probe writes to no device, and the only thing it writes at all
// is its own report.
//
// What it mounts is chosen for the same reason each time. The host's /sys
// carries the devices and the CPU topology, its /proc carries the huge pages
// and the mount table, and its /dev carries the nodes to open. hostNetwork is
// on so that /sys/class/net is the host's interfaces rather than the pod's,
// since the net subsystem is the one part of sysfs that a network namespace
// changes.

package nodeprobe

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ContainerName is the probe container's name, which is what a
	// kubectl logs -c has to name.
	ContainerName = "nodeprobe"

	// BinaryName is the probe's entry point inside the image.
	BinaryName = "simplyblock-nodeprobe"

	// The paths the host's trees are mounted at inside the pod. They are under
	// /host rather than at the root so that nothing shadows the container's own
	// /proc, which the Go runtime reads.
	hostSysfsMount = "/host/sys"
	hostProcMount  = "/host/proc"

	// hostDevMount is /dev and not /host/dev, because a device path is what the
	// report carries and a reviewer approves: a report naming
	// /host/dev/nvme0n1 would name a path that exists on no host.
	hostDevMount = "/dev"

	// HostMountinfoPath is the mount table the probe reads.
	//
	// It is PID 1's and not the probe's own. A pod has its own mount namespace,
	// so its own table lists none of the host's mounts: a probe reading it
	// would find nothing mounted and report the disk carrying the host's root
	// filesystem as free. PID 1 is in the host's mount namespace, and its table
	// is what the host has mounted.
	HostMountinfoPath = hostProcMount + "/1/mountinfo"

	// defaultTTLSeconds is how long a finished Job stays. It outlives the
	// discovery run that created it by enough for somebody to read its logs
	// after the run failed.
	defaultTTLSeconds int32 = 3600

	// defaultBackoffLimit is how many times a failed probe is retried. The
	// probe is read-only and deterministic, so a retry helps only against a
	// transient API error on the way to writing its report.
	defaultBackoffLimit int32 = 2

	// defaultDeadlineSeconds bounds one probe. Reading a fleet's worth of
	// devices is seconds of work, and a probe still running after this is one
	// stuck on a device that will not answer.
	defaultDeadlineSeconds int64 = 300
)

// JobOptions is what one probe Job needs to be built.
type JobOptions struct {
	// Namespace is where the Job and the report it writes go.
	Namespace string

	// Run is the discovery run this probe belongs to. It names the report and
	// labels every object, so two runs against one fleet do not overwrite each
	// other's answers.
	Run string

	// Node is the worker to probe, by its Kubernetes node name.
	//
	// The pod is pinned with spec.nodeName rather than a hostname selector,
	// because the node's name is what the caller has and the
	// kubernetes.io/hostname label is not required to equal it. Pinning that
	// way bypasses the scheduler, so which workers are worth probing —
	// schedulable, matching the run's selector — is decided before this is
	// called, and Tolerations covers the taints that remain.
	Node string

	// Image is the probe image, which is the operator's own: the probe binary
	// ships in it beside the manager, so a Job cannot be a version out of step
	// with the operator that created it. The caller passes the image it is
	// itself running.
	Image string

	// ImagePullPolicy defaults to IfNotPresent, which is right for an image
	// pinned by digest or by a release tag.
	ImagePullPolicy corev1.PullPolicy

	// ServiceAccountName is the account the probe writes its report as. It
	// needs create and update on ConfigMaps in Namespace and nothing else.
	ServiceAccountName string

	// Owner is what the Job and its report belong to, normally the OperatorOps
	// run. With it, deleting the run collects both.
	Owner *metav1.OwnerReference

	// Tolerations lets a probe run on a tainted worker. Empty is the
	// conservative default: a taint is usually a statement that the node is not
	// taking work, and a fleet that means to give a tainted worker to
	// simplyblock says so here.
	Tolerations []corev1.Toleration

	// TTLSecondsAfterFinished, BackoffLimit, and ActiveDeadlineSeconds override
	// the defaults above. Nil takes the default.
	TTLSecondsAfterFinished *int32
	BackoffLimit            *int32
	ActiveDeadlineSeconds   *int64
}

// Job builds the probe Job for one worker.
func Job(opts JobOptions) (*batchv1.Job, error) {
	switch {
	case opts.Namespace == "":
		return nil, fmt.Errorf("a probe Job needs a namespace")
	case opts.Run == "":
		return nil, fmt.Errorf("a probe Job needs the name of the run it belongs to")
	case opts.Node == "":
		return nil, fmt.Errorf("a probe Job needs the node to run on")
	case opts.Image == "":
		return nil, fmt.Errorf("a probe Job needs an image")
	case opts.ServiceAccountName == "":
		// Left empty, the pod runs as the namespace's default service account,
		// whose permissions are whatever that namespace happens to grant. The
		// probe is meant to run under an account with two verbs on one kind, so
		// the account is named rather than defaulted: a silent fallback is how
		// a probe ends up with more access than it was designed to have.
		return nil, fmt.Errorf(
			"a probe Job needs a service account, and defaulting it would run the probe " +
				"as the namespace's default account with whatever that grants")
	}

	privileged := true
	readOnlyRoot := true
	runAsUser := int64(0)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ObjectName(opts.Run, opts.Node),
			Namespace: opts.Namespace,
			Labels: map[string]string{
				LabelComponent: ComponentNodeProbe,
				LabelRun:       labelValue(opts.Run),
				LabelNode:      labelValue(opts.Node),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            valueOr(opts.BackoffLimit, defaultBackoffLimit),
			TTLSecondsAfterFinished: valueOr(opts.TTLSecondsAfterFinished, defaultTTLSeconds),
			ActiveDeadlineSeconds:   valueOr(opts.ActiveDeadlineSeconds, defaultDeadlineSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelComponent: ComponentNodeProbe,
						LabelRun:       labelValue(opts.Run),
						LabelNode:      labelValue(opts.Node),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					NodeName:           opts.Node,
					HostNetwork:        true,
					ServiceAccountName: opts.ServiceAccountName,
					Tolerations:        opts.Tolerations,
					Volumes: []corev1.Volume{
						hostPathVolume("host-sys", "/sys"),
						hostPathVolume("host-proc", "/proc"),
						hostPathVolume("host-dev", "/dev"),
					},
					Containers: []corev1.Container{{
						Name:            ContainerName,
						Image:           opts.Image,
						ImagePullPolicy: pullPolicyOr(opts.ImagePullPolicy),
						Command: []string{
							BinaryName,
							"--node=$(NODE_NAME)",
							"--namespace=$(POD_NAMESPACE)",
							"--run=" + opts.Run,
							"--sysfs-root=" + hostSysfsMount,
							"--proc-root=" + hostProcMount,
							"--dev-root=" + hostDevMount,
							"--mountinfo=" + HostMountinfoPath,
						},
						Env: append([]corev1.EnvVar{
							fieldRefEnv("NODE_NAME", "spec.nodeName"),
							fieldRefEnv("POD_NAMESPACE", "metadata.namespace"),
						}, ownerEnv(opts.Owner)...),
						SecurityContext: &corev1.SecurityContext{
							// Privileged for the device cgroup, and root
							// because reading PID 1's mount table needs the
							// same user PID 1 runs as. The root filesystem is
							// read-only anyway: nothing the probe does writes
							// to its own container.
							Privileged:             &privileged,
							RunAsUser:              &runAsUser,
							ReadOnlyRootFilesystem: &readOnlyRoot,
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "host-sys", MountPath: hostSysfsMount, ReadOnly: true},
							{Name: "host-proc", MountPath: hostProcMount, ReadOnly: true},
							// /dev is not read-only: opening a device node
							// O_EXCL is the kernel's own answer to whether
							// anything holds it, and that open needs the mount
							// to be writable even though the device is opened
							// for reading.
							{Name: "host-dev", MountPath: hostDevMount},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
						// A probe that failed says why in its logs, and the
						// last of them is what a kubectl describe of the pod
						// shows without anybody fetching them.
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
				},
			},
		},
	}
	if opts.Owner != nil {
		job.OwnerReferences = []metav1.OwnerReference{*opts.Owner}
	}
	return job, nil
}

// ownerEnv passes the owner reference the probe stamps on its report.
//
// It travels as environment rather than being looked up, so the probe needs no
// read access to the object that owns it: a run's name and UID are all the
// garbage collector needs, and the operator already has both.
func ownerEnv(owner *metav1.OwnerReference) []corev1.EnvVar {
	if owner == nil {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "OWNER_API_VERSION", Value: owner.APIVersion},
		{Name: "OWNER_KIND", Value: owner.Kind},
		{Name: "OWNER_NAME", Value: owner.Name},
		{Name: "OWNER_UID", Value: string(owner.UID)},
	}
}

// hostPathVolume is one of the host's trees, mounted into the pod.
func hostPathVolume(name, path string) corev1.Volume {
	kind := corev1.HostPathDirectory
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &kind},
		},
	}
}

// fieldRefEnv reads a fact about the pod out of its own spec, which is how the
// probe learns which node it landed on without asking the API.
func fieldRefEnv(name, path string) corev1.EnvVar {
	return corev1.EnvVar{
		Name:      name,
		ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}},
	}
}

func pullPolicyOr(policy corev1.PullPolicy) corev1.PullPolicy {
	if policy == "" {
		return corev1.PullIfNotPresent
	}
	return policy
}

// valueOr returns a pointer to the caller's value, or to the default when the
// caller gave none.
func valueOr[T any](given *T, fallback T) *T {
	if given != nil {
		return given
	}
	return &fallback
}
