// The Jobs a HostMaintenance window creates: one per fdbcli command, and the
// sentinel that makes a drain of the worker visible. Rows U-17 through U-22,
// U-75, U-89, and U-90 in test-plan-fdb-host-maintenance.md.

package controlplane

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const testWindowName = "simplyblock-maintenance-3f9a1c2e"

func aWindow(worker string) *simplyblockv1alpha2.ControlPlaneOps {
	return &simplyblockv1alpha2.ControlPlaneOps{
		ObjectMeta: metav1.ObjectMeta{Name: testWindowName, Namespace: "simplyblock", UID: "window-uid"},
		Spec: simplyblockv1alpha2.ControlPlaneOpsSpec{
			ControlPlaneRef: SingletonName,
			Action:          simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance,
			HostMaintenance: &simplyblockv1alpha2.HostMaintenanceSpec{WorkerNode: worker},
		},
	}
}

func aPlacedControlPlane() *simplyblockv1alpha2.ControlPlane {
	return &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{
				Local: &simplyblockv1alpha2.LocalControlPlane{
					Image:        testImage,
					NodeSelector: map[string]string{"role": "control"},
					Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}},
				},
			},
		},
	}
}

// U-17: the image, the mount, and the command.
func TestACommandJobRunsFdbcliAgainstTheMountedClusterFile(t *testing.T) {
	job := fdbCommandJob(aWindow("vm11"), aPlacedControlPlane(),
		fdbCommandMaintenanceOn, "maintenance on vm11 14400", false)

	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != image(fdbMonitorImage) {
		t.Errorf("image = %s, want the FoundationDB image %s", c.Image, image(fdbMonitorImage))
	}
	script := strings.Join(c.Command, " ")
	for _, want := range []string{"fdbcli", "-C " + clusterFilePath, "--timeout 30", "--exec"} {
		if !strings.Contains(script, want) {
			t.Errorf("command %q does not carry %q", script, want)
		}
	}
	if len(c.Args) != 1 || c.Args[0] != "maintenance on vm11 14400" {
		t.Errorf("args = %v, want the fdbcli command as the one argument", c.Args)
	}
	mounted := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == clusterFilePath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("mounts = %v, the cluster file is not at %s", c.VolumeMounts, clusterFilePath)
	}
	if len(job.Spec.Template.Spec.Volumes) == 0 ||
		job.Spec.Template.Spec.Volumes[0].ConfigMap == nil ||
		job.Spec.Template.Spec.Volumes[0].ConfigMap.Name != clusterFileConfigMapName {
		t.Errorf("volumes = %v, want the cluster-file ConfigMap", job.Spec.Template.Spec.Volumes)
	}
}

// U-18: placement and the Job's own limits.
func TestACommandJobTakesTheDatabasesPlacementAndItsLimits(t *testing.T) {
	job := fdbCommandJob(aWindow("vm11"), aPlacedControlPlane(),
		fdbCommandCoordinators, "coordinators a:4501 b:4501 c:4501", false)
	spec := job.Spec.Template.Spec

	if spec.NodeSelector["role"] != "control" {
		t.Errorf("nodeSelector = %v, want the database's", spec.NodeSelector)
	}
	if len(spec.Tolerations) == 0 || spec.Tolerations[0].Key != "dedicated" {
		t.Errorf("tolerations = %v, want the database's first", spec.Tolerations)
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s, want Never", spec.RestartPolicy)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 3 {
		t.Errorf("backoffLimit = %v, want 3", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 120 {
		t.Errorf("activeDeadlineSeconds = %v, want 120", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("ttlSecondsAfterFinished = %v, want 3600", job.Spec.TTLSecondsAfterFinished)
	}
}

// U-19: the operation owns its Jobs.
func TestACommandJobIsOwnedByTheWindow(t *testing.T) {
	window := aWindow("vm11")
	for _, job := range []*batchv1.Job{
		fdbCommandJob(window, aPlacedControlPlane(), fdbCommandMaintenanceOff, "maintenance off", false),
		sentinelJob(window, aPlacedControlPlane()),
	} {
		refs := job.OwnerReferences
		if len(refs) != 1 || refs[0].Name != window.Name || refs[0].UID != window.UID ||
			refs[0].Kind != "ControlPlaneOps" || refs[0].Controller == nil || !*refs[0].Controller {
			t.Errorf("%s ownerReferences = %v, want the window as controller", job.Name, refs)
		}
	}
}

// U-20: a Job that exists is read, not recreated.
func TestEnsureJobCreatesOnceAndReadsAfterward(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	window := aWindow("vm11")
	desired := fdbCommandJob(window, aPlacedControlPlane(), fdbCommandMaintenanceOn, "maintenance on vm11 14400", false)

	first, err := ensureJob(ctx, c, desired)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// A later pass builds the same desired Job and finds the first one.
	again := fdbCommandJob(window, aPlacedControlPlane(), fdbCommandMaintenanceOn, "maintenance on vm11 14400", false)
	second, err := ensureJob(ctx, c, again)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if first.Name != second.Name || first.UID != second.UID {
		t.Errorf("the second pass got %s/%s, want the first Job %s/%s back", second.Name, second.UID, first.Name, first.UID)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace("simplyblock")); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Errorf("%d Jobs exist, want exactly one", len(jobs.Items))
	}
}

// U-21: the name stays a DNS label however long the operation's name is.
func TestJobNamesAreDNSLabels(t *testing.T) {
	for _, ops := range []string{
		testWindowName,
		strings.Repeat("a-very-long-control-plane-name", 2) + "-maintenance-3f9a1c2e",
	} {
		for _, cmd := range []fdbCommand{fdbCommandMaintenanceOn, fdbCommandCoordinators, fdbCommandMaintenanceStatus, fdbCommandMaintenanceOff, fdbCommandSentinel} {
			name := fdbJobName(ops, cmd)
			if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
				t.Errorf("fdbJobName(%q, %s) = %q: %v", ops, cmd, name, errs)
			}
			if !strings.HasSuffix(name, "-"+string(cmd)) {
				t.Errorf("fdbJobName(%q, %s) = %q, want the command as the suffix", ops, cmd, name)
			}
		}
	}
}

// U-22: what a Job's conditions mean to a step, read by the package's existing
// jobOutcome, which the index Job already uses.
func TestAJobIsRunningCompleteOrFailed(t *testing.T) {
	cases := map[string]struct {
		conditions []batchv1.JobCondition
		want       outcome
	}{
		"no condition":   {nil, jobRunning},
		"complete":       {[]batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}, jobSucceeded},
		"failed":         {[]batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}, jobFailed},
		"complete false": {[]batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}}, jobRunning},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: tc.conditions}}
			if got := jobOutcome(job); got != tc.want {
				t.Errorf("outcome = %d, want %d", got, tc.want)
			}
		})
	}
}

// The one command whose output matters is read from the termination message
// the script writes, so no log access is needed.
func TestTheTerminationMessageOfAJobsPodIsRead(t *testing.T) {
	ctx := context.Background()
	job := fdbCommandJob(aWindow("vm11"), aPlacedControlPlane(),
		fdbCommandMaintenanceStatus, "maintenance status", false)
	job.UID = "job-uid"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-abcde", Namespace: job.Namespace,
			Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: "Maintenance for zone vm11 will continue for 14000.0 seconds.",
			}},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()

	got, err := jobTerminationMessage(ctx, c, job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "zone vm11") {
		t.Errorf("message = %q, want the pod's termination message", got)
	}
}

// U-75: the sentinel.
func TestTheSentinelIsAPinnedPauseJobNothingRecreates(t *testing.T) {
	job := sentinelJob(aWindow("vm13"), aPlacedControlPlane())
	spec := job.Spec.Template.Spec

	if job.Name != testWindowName+"-sentinel" {
		t.Errorf("name = %s", job.Name)
	}
	if spec.NodeName != "vm13" {
		t.Errorf("nodeName = %q, want the worker", spec.NodeName)
	}
	if spec.Containers[0].Image != image(fdbMonitorImage) {
		t.Errorf("image = %s, want the FoundationDB image every host has", spec.Containers[0].Image)
	}
	if strings.Join(spec.Containers[0].Command, " ") != "sleep infinity" {
		t.Errorf("command = %v, want sleep infinity", spec.Containers[0].Command)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0 so nothing recreates the pod", job.Spec.BackoffLimit)
	}
	if _, held := job.Spec.Template.Labels[fdbMaintenanceLabel]; held {
		t.Error("the sentinel carries the budget's label, so the budget would hold it")
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s, want Never", spec.RestartPolicy)
	}
}

// U-89: the command Job runs while the pool is cordoned, and away from the worker.
func TestACommandJobToleratesTheCordonAndAvoidsTheWorker(t *testing.T) {
	job := fdbCommandJob(aWindow("vm11"), aPlacedControlPlane(),
		fdbCommandMaintenanceOn, "maintenance on vm11 14400", false)
	spec := job.Spec.Template.Spec

	tolerated := false
	for _, tol := range spec.Tolerations {
		if tol.Key == corev1.TaintNodeUnschedulable && tol.Effect == corev1.TaintEffectNoSchedule {
			tolerated = true
		}
	}
	if !tolerated {
		t.Errorf("tolerations = %v, the cordon taint is not tolerated", spec.Tolerations)
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil ||
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatal("no required node affinity, so the Job may land on the worker being drained")
	}
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	away := false
	for _, term := range terms {
		for _, expr := range term.MatchFields {
			if expr.Key == "metadata.name" && expr.Operator == corev1.NodeSelectorOpNotIn && len(expr.Values) == 1 && expr.Values[0] == "vm11" {
				away = true
			}
		}
	}
	if !away {
		t.Errorf("affinity = %v, want metadata.name NotIn [vm11]", terms)
	}
}

// U-90: on a cluster where the worker is the only node, the affinity is dropped.
func TestACommandJobOnTheOnlyNodeCarriesNoAffinity(t *testing.T) {
	job := fdbCommandJob(aWindow("vm11"), aPlacedControlPlane(),
		fdbCommandMaintenanceOn, "maintenance on vm11 14400", true)
	if job.Spec.Template.Spec.Affinity != nil {
		t.Errorf("affinity = %v, want none when there is nowhere else to run", job.Spec.Template.Spec.Affinity)
	}
}
