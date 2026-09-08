// What the probe Job's pod spec has to say, and why each of those things is
// asserted rather than left to review.
//
// Three of them decide whether the probe's answer is true at all. The mount
// table has to be the host's, or a pod finds nothing mounted and reports the
// root disk as free. The container has to be privileged, or the device cgroup
// denies every disk and the probe reports a fleet it could not read. And the
// pod has to land on the node it is named for, or a report is attributed to a
// machine it does not describe.

package nodeprobe

import (
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testNode and testRun are the worker and the run every case below is about.
const (
	testNode = "worker-3"
	testRun  = "oops-20260908"
)

// probeJob builds the Job a discovery run would create for one worker.
func probeJob(t *testing.T) *batchv1.Job {
	t.Helper()
	job, err := Job(JobOptions{
		Namespace:          "simplyblock",
		Run:                testRun,
		Node:               testNode,
		Image:              "quay.io/simplyblock-io/simplyblock-operator:26.4.0",
		ServiceAccountName: "simplyblock-nodeprobe",
	})
	if err != nil {
		t.Fatalf("build the probe Job: %v", err)
	}
	return job
}

// container is the Job's single container.
func container(t *testing.T, job *batchv1.Job) corev1.Container {
	t.Helper()
	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("the pod has %d containers, want 1", len(containers))
	}
	return containers[0]
}

// arg reports whether the container's command carries a flag.
func arg(c corev1.Container, flag string) bool {
	return slices.ContainsFunc(c.Command, func(a string) bool { return strings.HasPrefix(a, flag) })
}

func TestJobReadsTheHostsMountTableAndNotThePodsOwn(t *testing.T) {
	// A pod has its own mount namespace, so its own mountinfo lists none of the
	// host's mounts: a probe reading it concludes nothing is mounted and
	// reports the disk carrying the host's root filesystem as free. PID 1 is in
	// the host's namespace, and its table is what the host has mounted.
	c := container(t, probeJob(t))

	want := "--mountinfo=" + HostMountinfoPath
	if !slices.Contains(c.Command, want) {
		t.Fatalf("the command is %v, and it does not carry %s", c.Command, want)
	}
	if !strings.HasSuffix(HostMountinfoPath, "/1/mountinfo") {
		t.Errorf("the mount table is %s, and it has to be PID 1's", HostMountinfoPath)
	}
	if strings.Contains(HostMountinfoPath, "/self/") {
		t.Errorf("the mount table is %s, which is the pod's own", HostMountinfoPath)
	}
}

func TestJobIsPrivilegedAndSaysWhyByWhatItMounts(t *testing.T) {
	// A container's device cgroup denies opening every block device the kubelet
	// did not grant it, so an unprivileged probe reads sysfs and then not one
	// disk. The reading it would be missing is the one that decides whether
	// simplyblock would overwrite somebody's data.
	c := container(t, probeJob(t))

	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("the probe container is not privileged, so it cannot open a single disk")
	}
	if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Error("the probe does not run as root, and reading PID 1's mount table " +
			"needs the user PID 1 runs as")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the probe's own root filesystem is writable, and it writes nothing to it")
	}
}

func TestJobMountsTheHostsTreesReadOnlyExceptTheDeviceNodes(t *testing.T) {
	c := container(t, probeJob(t))

	byPath := map[string]corev1.VolumeMount{}
	for _, mount := range c.VolumeMounts {
		byPath[mount.MountPath] = mount
	}

	for _, path := range []string{hostSysfsMount, hostProcMount} {
		mount, ok := byPath[path]
		if !ok {
			t.Errorf("the host's tree at %s is not mounted", path)
			continue
		}
		if !mount.ReadOnly {
			t.Errorf("%s is mounted writable, and the probe only reads it", path)
		}
	}

	// /dev is the exception: asking the kernel whether anything holds a device
	// is an O_EXCL open, and that needs the mount to be writable even though
	// the device itself is opened for reading.
	dev, ok := byPath[hostDevMount]
	if !ok {
		t.Fatalf("the host's device nodes are not mounted at %s", hostDevMount)
	}
	if dev.ReadOnly {
		t.Error("/dev is mounted read-only, and the exclusive open the usage " +
			"reading depends on cannot be made through it")
	}

	// The device paths in the report are the host's, so the mount point has to
	// be the one a host uses: /host/dev/nvme0n1 names a path that exists
	// nowhere.
	if hostDevMount != "/dev" {
		t.Errorf("the device nodes are mounted at %s, and a report has to name the host's paths", hostDevMount)
	}
}

func TestJobLandsOnTheNodeItIsNamedFor(t *testing.T) {
	// A report is attributed to the node the probe was told to read, so a pod
	// that landed anywhere else would attribute one machine's disks to another.
	// The pin is spec.nodeName and not a hostname selector, because the node's
	// name is what the caller has and the kubernetes.io/hostname label is not
	// required to equal it.
	spec := probeJob(t).Spec.Template.Spec

	if spec.NodeName != testNode {
		t.Errorf("the pod is pinned to %q, want %s", spec.NodeName, testNode)
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("the restart policy is %q, and a one-shot probe is retried by its Job", spec.RestartPolicy)
	}
	if !spec.HostNetwork {
		t.Error("the pod does not use the host's network, so /sys/class/net would " +
			"report the pod's interfaces rather than the worker's")
	}
}

func TestJobPassesTheNodeAndNamespaceOutOfThePodsOwnSpec(t *testing.T) {
	// The probe learns which node it landed on from its own spec rather than by
	// asking the API, which is what keeps its permissions to one verb on one
	// kind.
	c := container(t, probeJob(t))

	byName := map[string]corev1.EnvVar{}
	for _, env := range c.Env {
		byName[env.Name] = env
	}
	for name, want := range map[string]string{
		"NODE_NAME":     "spec.nodeName",
		"POD_NAMESPACE": "metadata.namespace",
	} {
		env, ok := byName[name]
		if !ok {
			t.Errorf("%s is not passed to the probe", name)
			continue
		}
		if env.ValueFrom == nil || env.ValueFrom.FieldRef == nil || env.ValueFrom.FieldRef.FieldPath != want {
			t.Errorf("%s comes from %+v, want a field reference to %s", name, env.ValueFrom, want)
		}
	}
	for _, flag := range []string{"--node=", "--namespace=", "--run=", "--sysfs-root=", "--proc-root=", "--dev-root="} {
		if !arg(c, flag) {
			t.Errorf("the command %v does not carry %s", c.Command, flag)
		}
	}
}

func TestJobPassesAnOwnerSoTheReportIsCollectedWithTheRun(t *testing.T) {
	owner := &metav1.OwnerReference{
		APIVersion: "storage.simplyblock.io/v1alpha1",
		Kind:       "OperatorOps",
		Name:       testRun,
		UID:        "8f14e45f-ceea-467a-9d1f-2e0b1c4b6b8a",
	}

	job, err := Job(JobOptions{
		Namespace: "simplyblock", Run: testRun, Node: testNode,
		Image: "operator:26.4.0", Owner: owner,
	})
	if err != nil {
		t.Fatalf("build the probe Job: %v", err)
	}

	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != owner.UID {
		t.Errorf("the Job's owner references are %+v, want the run", job.OwnerReferences)
	}

	// The probe stamps the same owner on the ConfigMap it writes, and it is
	// given the reference rather than looking one up so it needs no read access
	// to the run.
	byName := map[string]string{}
	for _, env := range container(t, job).Env {
		byName[env.Name] = env.Value
	}
	for name, want := range map[string]string{
		"OWNER_API_VERSION": owner.APIVersion,
		"OWNER_KIND":        owner.Kind,
		"OWNER_NAME":        owner.Name,
		"OWNER_UID":         string(owner.UID),
	} {
		if byName[name] != want {
			t.Errorf("%s is %q, want %q", name, byName[name], want)
		}
	}
}

func TestJobPassesNoOwnerWhenItWasGivenNone(t *testing.T) {
	// A partial owner reference is one the API server rejects, so it is all
	// four parts or none.
	c := container(t, probeJob(t))
	for _, env := range c.Env {
		if strings.HasPrefix(env.Name, "OWNER_") {
			t.Errorf("%s is passed for a Job with no owner", env.Name)
		}
	}
}

func TestJobIsBoundedInTimeAndInRetries(t *testing.T) {
	// A probe left running is a discovery step with nothing to wait on, and one
	// retried forever is a step that never fails.
	spec := probeJob(t).Spec

	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds <= 0 {
		t.Error("the probe has no deadline, so a read that never returns holds the run open")
	}
	if spec.BackoffLimit == nil {
		t.Error("the probe has no backoff limit")
	}
	if spec.TTLSecondsAfterFinished == nil || *spec.TTLSecondsAfterFinished <= 0 {
		t.Error("a finished probe Job is never collected")
	}
}

func TestJobHonorsTheOverridesItWasGiven(t *testing.T) {
	ttl, backoff, deadline := int32(60), int32(0), int64(30)
	tolerations := []corev1.Toleration{{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists}}

	job, err := Job(JobOptions{
		Namespace: "simplyblock", Run: "oops-1", Node: testNode, Image: "operator:26.4.0",
		TTLSecondsAfterFinished: &ttl,
		BackoffLimit:            &backoff,
		ActiveDeadlineSeconds:   &deadline,
		Tolerations:             tolerations,
		ImagePullPolicy:         corev1.PullAlways,
	})
	if err != nil {
		t.Fatalf("build the probe Job: %v", err)
	}

	if *job.Spec.TTLSecondsAfterFinished != ttl || *job.Spec.BackoffLimit != backoff ||
		*job.Spec.ActiveDeadlineSeconds != deadline {
		t.Errorf("the overrides were not applied: %+v", job.Spec)
	}
	if len(job.Spec.Template.Spec.Tolerations) != 1 {
		t.Errorf("the tolerations are %+v", job.Spec.Template.Spec.Tolerations)
	}
	if container(t, job).ImagePullPolicy != corev1.PullAlways {
		t.Errorf("the pull policy is %q, want Always", container(t, job).ImagePullPolicy)
	}
}

func TestJobDefaultsToNoTolerations(t *testing.T) {
	// A taint is usually a statement that the node is not taking work, and a
	// probe that tolerated everything would inspect the control plane.
	if tolerations := probeJob(t).Spec.Template.Spec.Tolerations; len(tolerations) != 0 {
		t.Errorf("the probe tolerates %+v by default", tolerations)
	}
}

func TestJobRefusesWhatItCannotBuild(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts JobOptions
	}{
		{"no namespace", JobOptions{Run: "r", Node: "n", Image: "i"}},
		{"no run", JobOptions{Namespace: "ns", Node: "n", Image: "i"}},
		{"no node", JobOptions{Namespace: "ns", Run: "r", Image: "i"}},
		{"no image", JobOptions{Namespace: "ns", Run: "r", Node: "n"}},
	} {
		if _, err := Job(tc.opts); err == nil {
			t.Errorf("%s: built a Job anyway", tc.name)
		}
	}
}

func TestJobAndItsReportShareTheirName(t *testing.T) {
	// One name per run and node, so the discovery step that found the Job knows
	// which ConfigMap to read without a second lookup.
	job := probeJob(t)
	if job.Name != ObjectName(testRun, testNode) {
		t.Errorf("the Job is called %q and its report %q",
			job.Name, ObjectName(testRun, testNode))
	}
	for key, want := range ReportSelector(testRun) {
		if job.Labels[key] != want {
			t.Errorf("the Job's label %s is %q, want %q", key, job.Labels[key], want)
		}
	}
}
