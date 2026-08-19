package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// ---- job naming ----

// Job names are Kubernetes object names, so a node name that is long, upper-case or
// dotted must still produce a valid DNS-1123 label — and two different nodes must
// never collide, or the second node's validation would silently reuse the first's Job.
func TestNodeSuffix(t *testing.T) {
	label := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	cases := []string{
		"worker-1",
		"vm04.simplyblock4.localdomain",
		"WORKER-UPPER.example.com",
		"ip-10-0-1-23.eu-central-1.compute.internal",
		strings.Repeat("very-long-node-name", 10),
		"node_with_underscores",
		"1.2.3.4",
		"",
	}
	seen := map[string]string{}
	for _, node := range cases {
		t.Run(node, func(t *testing.T) {
			suffix := nodeSuffix(node)
			if !label.MatchString(suffix) {
				t.Errorf("nodeSuffix(%q) = %q, which is not a valid DNS-1123 label", node, suffix)
			}
			full := "vmig-validate-" + safeNodeID(testMigrationUUID) + "-" + suffix
			if len(full) > 63 {
				t.Errorf("job name %q is %d chars, over the 63-char label limit", full, len(full))
			}
			if other, dup := seen[suffix]; dup {
				t.Errorf("nodeSuffix(%q) collides with nodeSuffix(%q) = %q", node, other, suffix)
			}
			seen[suffix] = node
		})
	}

	// Same node, same suffix: the Job is recreated under the same name across
	// reconciles rather than piling up duplicates.
	if a, b := nodeSuffix("worker-1"), nodeSuffix("worker-1"); a != b {
		t.Errorf("nodeSuffix is not deterministic: %q vs %q", a, b)
	}
	// Hosts that share a short name but differ in domain must not collide.
	if a, b := nodeSuffix("vm04.dc1.example.com"), nodeSuffix("vm04.dc2.example.com"); a == b {
		t.Errorf("nodeSuffix collides for different FQDNs with the same host part: %q", a)
	}
}

// ---- connection conversion ----

// The Job receives these as JSON and passes them to `nvme connect`; a dropped field
// changes the resulting path's behaviour, so the mapping is asserted whole.
func TestConnectionsToValidation(t *testing.T) {
	in := []simplyblockv1alpha1.MigrationConnection{{
		NQN: "nqn.x", IP: "10.0.0.1", Port: 4420, Transport: "tcp",
		NrIoQueues: 3, ReconnectDelay: 2, CtrlLossTmo: 3600, FastIOFailTmo: 8, KeepAliveTmo: 4,
	}}
	got := connectionsToValidation(in)
	if len(got) != 1 {
		t.Fatalf("got %d connections, want 1", len(got))
	}
	want := vmigration.Connection{
		NQN: "nqn.x", IP: "10.0.0.1", Port: 4420, Transport: "tcp",
		NrIoQueues: 3, ReconnectDelay: 2, CtrlLossTmo: 3600, FastIOFailTmo: 8, KeepAliveTmo: 4,
	}
	if got[0] != want {
		t.Errorf("connection = %+v, want %+v", got[0], want)
	}
	if out := connectionsToValidation(nil); len(out) != 0 {
		t.Errorf("connectionsToValidation(nil) = %+v, want empty", out)
	}
}

// ---- PV lookup by volume UUID ----

func TestPVNamesForVolumes(t *testing.T) {
	hostPath := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-hostpath"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/data"},
			},
		},
	}
	malformed := csiPV("not-a-handle")
	malformed.Name = "pv-malformed"
	wanted := csiPV(testClusterUUID + ":" + testPoolUUID + ":" + testVolumeUUID)
	other := csiPV(testClusterUUID + ":" + testPoolUUID + ":other-vol")
	other.Name = "pv-other"

	r, _ := newVMReconciler(t, unreachableAPI, hostPath, malformed, wanted, other)

	got, err := r.pvNamesForVolumes(context.Background(),
		map[string]struct{}{testVolumeUUID: {}, "missing-vol": {}})
	if err != nil {
		t.Fatalf("pvNamesForVolumes: %v", err)
	}
	// Non-CSI and malformed handles are skipped rather than fatal — a cluster can hold
	// PVs from any driver, and one bad handle must not stop the resolution.
	if len(got) != 1 || got[0] != testPVName {
		t.Errorf("PVs = %v, want just %s", got, testPVName)
	}
}

// ---- consumer resolution failures ----

// A PV that vanished between the member listing and the consumer lookup is an error
// worth retrying, not a silent "no consumer" that would skip validation for it.
func TestResolveConsumerNodeName_PVMissing(t *testing.T) {
	r, _ := newVMReconciler(t, unreachableAPI)
	if _, err := r.resolveConsumerNodeName(context.Background(), "pv-does-not-exist"); err == nil {
		t.Errorf("expected an error for a missing PV")
	}
}

// ---- rebalancer image resolution ----

func TestResolveRebalancerImage(t *testing.T) {
	enabled, disabled := true, false
	image := "pinned:v1"

	t.Run("explicit image is used", func(t *testing.T) {
		r, _ := newVMReconciler(t, unreachableAPI, clusterWithSettings(
			&simplyblockv1alpha1.VolumeMigrationSettings{Enabled: &enabled, RebalancerImage: &image}))
		got, err := r.resolveRebalancerImage(context.Background(), testVMNamespace, testClusterUUID)
		if err != nil {
			t.Fatalf("resolveRebalancerImage: %v", err)
		}
		if got != image {
			t.Errorf("image = %q, want %q", got, image)
		}
	})

	t.Run("enabled without a pinned image falls back to the default", func(t *testing.T) {
		r, _ := newVMReconciler(t, unreachableAPI, clusterWithSettings(
			&simplyblockv1alpha1.VolumeMigrationSettings{Enabled: &enabled}))
		got, err := r.resolveRebalancerImage(context.Background(), testVMNamespace, testClusterUUID)
		if err != nil {
			t.Fatalf("resolveRebalancerImage: %v", err)
		}
		if got == "" {
			t.Errorf("image is empty, want the default")
		}
	})

	t.Run("disabled is an error", func(t *testing.T) {
		r, _ := newVMReconciler(t, unreachableAPI, clusterWithSettings(
			&simplyblockv1alpha1.VolumeMigrationSettings{Enabled: &disabled}))
		_, err := r.resolveRebalancerImage(context.Background(), testVMNamespace, testClusterUUID)
		if err == nil {
			t.Fatalf("expected an error when migration is disabled")
		}
		if !strings.Contains(err.Error(), "disabled") {
			t.Errorf("error = %q, want it to say migration is disabled", err)
		}
	})

	// An unknown cluster UUID must not silently fall back to a default image: the CR
	// would then migrate against a cluster the operator does not manage.
	t.Run("no StorageCluster for the UUID is an error", func(t *testing.T) {
		r, _ := newVMReconciler(t, unreachableAPI)
		_, err := r.resolveRebalancerImage(context.Background(), testVMNamespace, "unknown-cluster")
		if err == nil {
			t.Fatalf("expected an error for an unknown cluster UUID")
		}
		if !strings.Contains(err.Error(), "unknown-cluster") {
			t.Errorf("error = %q, want it to name the cluster", err)
		}
	})
}

// ---- validation job bookkeeping ----

// The Job objects are created once per node; a re-created Job (same name) must not
// duplicate the status entry or fail the reconcile.
func TestStartValidationJobs_ExistingJobIsNotDuplicated(t *testing.T) {
	vm := validatingVM("")
	r, cl := newVMReconciler(t, unreachableAPI, vm, migrationCluster())

	if _, err := r.startValidationJobs(context.Background(), vm, []string{testConsumerNode}); err != nil {
		t.Fatalf("startValidationJobs: %v", err)
	}
	first := len(vm.Status.ValidationJobs)

	// Same node again: nothing new to create, nothing new to record.
	if _, err := r.startValidationJobs(context.Background(), vm, []string{testConsumerNode}); err != nil {
		t.Fatalf("startValidationJobs (repeat): %v", err)
	}
	if len(vm.Status.ValidationJobs) != first {
		t.Errorf("ValidationJobs = %+v, want no duplicate for the same node", vm.Status.ValidationJobs)
	}
	var jobs batchv1.JobList
	if err := cl.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Errorf("created %d jobs, want 1", len(jobs.Items))
	}
}

// With several nodes in flight, one Job disappearing must only drop that node's entry —
// the others keep their verdicts instead of the whole set being rebuilt.
func TestPollValidationJobs_OneJobVanished_KeepsTheOthers(t *testing.T) {
	vm := validatingVM(testValidationJobName)
	vm.Status.ValidationJobs = append(vm.Status.ValidationJobs,
		simplyblockv1alpha1.ValidationJob{Node: testSiblingNode, JobName: "vmig-validate-gone"})
	// Only the first node's Job exists.
	present := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})

	r, cl := newVMReconciler(t, unreachableAPI, vm, present, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if len(got.Status.ValidationJobs) != 1 || got.Status.ValidationJobs[0].JobName != testValidationJobName {
		t.Errorf("ValidationJobs = %+v, want only the vanished entry dropped", got.Status.ValidationJobs)
	}
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating", got.Status.Phase)
	}
}

// ---- releasing migration target paths ----

// releasingVM is a validating migration with target connections recorded on two nodes —
// the state in which giving up leaves NVMe paths behind on both.
func releasingVM() *simplyblockv1alpha1.VolumeMigration {
	vm := validatingVM(testValidationJobName)
	vm.Status.ValidationJobs = append(vm.Status.ValidationJobs,
		simplyblockv1alpha1.ValidationJob{Node: testSiblingNode, JobName: "vmig-validate-2"})
	vm.Status.Connections = []simplyblockv1alpha1.MigrationConnection{
		{NQN: testSubsystemNQN, IP: "10.0.0.1", Port: 4428, Transport: "tcp"},
	}
	return vm
}

// releaseJobs returns the release Jobs that exist, by the node each is pinned to.
func releaseJobs(t *testing.T, cl client.Client) map[string]batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := cl.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	out := map[string]batchv1.Job{}
	for _, j := range jobs.Items {
		if !strings.HasPrefix(j.Name, "vmig-release-") {
			continue
		}
		out[j.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]] = j
	}
	return out
}

// The gap a per-node release cannot close: a node whose own validation passed exits
// successfully and is never told the migration was cancelled, so the operator has to
// release for it. Every recorded node is asked, not only the ones that passed.
func TestReconcileAbort_ReleasesTargetPathsOnEveryNode(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	vm := releasingVM()
	vm.Spec.Abort = true
	r, cl := newVMReconciler(t, srv.URL, vm, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := releaseJobs(t, cl)
	for _, node := range []string{testValidationNode, testSiblingNode} {
		if _, ok := got[node]; !ok {
			t.Errorf("no release job pinned to %q; got %v", node, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("created %d release jobs, want one per validated node", len(got))
	}
}

// A failed validation cancels the migration, and the nodes that passed still hold their
// target paths — the same release has to happen on that route too, not only on abort.
func TestCancelAndFail_ReleasesTargetPaths(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	vm := releasingVM()
	r, cl := newVMReconciler(t, srv.URL, vm, migrationCluster())

	if _, err := r.cancelAndFail(context.Background(), vm, "NVMe path validation failed"); err != nil {
		t.Fatalf("cancelAndFail: %v", err)
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if got := releaseJobs(t, cl); len(got) != 2 {
		t.Errorf("created %d release jobs, want one per validated node", len(got))
	}
}

// The release Job has to run the release mode against the host's own fabric — a Job that
// looked at the container's /sys, or ran the validation mode, would either see nothing or
// connect the very paths it was sent to take back.
func TestBuildReleaseJob_RunsReleaseModeAgainstTheHost(t *testing.T) {
	r, _ := newVMReconciler(t, unreachableAPI)
	vm := releasingVM()

	job := r.buildReleaseJob(vm, testConsumerNode, "img:tag")
	c := job.Spec.Template.Spec.Containers[0]

	if !slices.Contains(c.Command, "--mode=release-migration-paths") {
		t.Errorf("command = %v, want the release mode", c.Command)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["VMIG_SUBSYSTEM_NQN"] != testSubsystemNQN {
		t.Errorf("VMIG_SUBSYSTEM_NQN = %q, want the migrated subsystem", env["VMIG_SUBSYSTEM_NQN"])
	}
	if env["VMIG_SYS_ROOT"] != "/host/sys" {
		t.Errorf("VMIG_SYS_ROOT = %q, want the mounted host sysfs", env["VMIG_SYS_ROOT"])
	}
	if !strings.Contains(env["VMIG_CONNECTIONS"], "10.0.0.1") {
		t.Errorf("VMIG_CONNECTIONS = %q, want the migration's target addresses", env["VMIG_CONNECTIONS"])
	}

	// The name must not collide with the validation Job of the same migration and node —
	// they exist at the same time on the abort path.
	if job.Name == r.buildValidationJob(vm, testConsumerNode, "img:tag").Name {
		t.Errorf("release and validation jobs share the name %q", job.Name)
	}
	// Unlike validation, a release is retried: nothing waits on it, so a transient
	// failure that is not retried is just a path left connected.
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit == 0 {
		t.Errorf("BackoffLimit = %v, want a retry budget", job.Spec.BackoffLimit)
	}
}

// Nothing to release when nothing was connected: a migration that never recorded target
// connections must not spawn Jobs to take back paths that do not exist.
func TestReleaseMigrationPaths_NoConnectionsStartsNoJobs(t *testing.T) {
	r, cl := newVMReconciler(t, unreachableAPI, migrationCluster())
	vm := releasingVM()
	vm.Status.Connections = nil

	r.releaseMigrationPaths(context.Background(), vm)

	if got := releaseJobs(t, cl); len(got) != 0 {
		t.Errorf("created %d release jobs with no recorded connections, want none", len(got))
	}
}

// ---- abort ----

// Aborting during validation must clean up every node's Job, not just the first.
func TestReconcileAbort_DeletesAllValidationJobs(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/migrations/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	vm := validatingVM(testValidationJobName)
	vm.Spec.Abort = true
	vm.Status.ValidationJobs = append(vm.Status.ValidationJobs,
		simplyblockv1alpha1.ValidationJob{Node: testSiblingNode, JobName: "vmig-validate-2"})
	job1 := validationJob()
	job2 := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "vmig-validate-2", Namespace: testVMNamespace}}

	r, cl := newVMReconciler(t, srv.URL, vm, job1, job2)

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseAborted {
		t.Fatalf("phase = %q, want Aborted", got.Status.Phase)
	}
	if len(got.Status.ValidationJobs) != 0 {
		t.Errorf("ValidationJobs = %+v, want cleared", got.Status.ValidationJobs)
	}
	for _, name := range []string{testValidationJobName, "vmig-validate-2"} {
		err := cl.Get(context.Background(),
			types.NamespacedName{Namespace: testVMNamespace, Name: name}, &batchv1.Job{})
		if err == nil {
			t.Errorf("job %q still exists after abort", name)
		}
	}
}

// A cancel that fails must not leave the CR claiming it was aborted: retry instead, so
// the backend migration is not left running behind an Aborted CR.
func TestReconcileAbort_CancelFails_RequeuesWithoutAborting(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	vm := validatingVM(testValidationJobName)
	vm.Spec.Abort = true
	r, cl := newVMReconciler(t, srv.URL, vm)

	res, err := r.Reconcile(context.Background(), vmRequest())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a retry", res.RequeueAfter)
	}
	if got := getVM(t, cl); got.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseAborted {
		t.Errorf("phase = Aborted although the backend cancel failed")
	}
}

// A migration deferred by a busy cluster has not been submitted, so aborting it needs
// no backend call — and must not spin for the rest of the deferral window.
func TestReconcileAbort_WhileDeferred_AbortsWithoutBackendCall(t *testing.T) {
	srv := newAPIServer(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: nothing was submitted to cancel", r.Method, r.URL.Path)
	})

	vm := baseVM()
	vm.Spec.Abort = true
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhasePending
	deferredSince := metav1.Now()
	vm.Status.DeferredSince = &deferredSince
	vm.Status.ClusterUUID = testClusterUUID
	// No MigrationUUID: the create never succeeded.

	r, cl := newVMReconciler(t, srv.URL, vm)
	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseAborted {
		t.Errorf("phase = %q, want Aborted", got.Status.Phase)
	}
	if got.Status.CompletedAt == nil {
		t.Errorf("CompletedAt must be stamped on abort")
	}
}

// Log collection is best effort and runs on every finished Job: a Job with no pods
// left (evicted, GC'd) must not disturb the reconcile.
func TestCollectAndLogJobPodLogs_NoPods(t *testing.T) {
	r, _ := newVMReconciler(t, unreachableAPI)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "vmig-validate-x", Namespace: testVMNamespace}}
	r.collectAndLogJobPodLogs(context.Background(), job) // must not panic
}

// Regression test for a validation loop that never converged: each Job was deleted the
// moment it passed while its status entry stayed, so the next pass read NotFound,
// called the Job "vanished", dropped the entry and rebuilt it — endlessly. Worse, the
// shrinking entry list let the gate declare "all validation jobs succeeded" for a
// subset, cutting over with an unvalidated node.
//
// A node that passed must stay recorded as passed, its Job must not be recreated, and
// the migration must not continue while another node is still pending.
func TestPollValidationJobs_PassedNodeIsNotRevalidated(t *testing.T) {
	srv := newAPIServer(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: one node is still pending", r.Method, r.URL.Path)
	})

	vm := validatingVM(testValidationJobName)
	vm.Status.ValidationJobs = []simplyblockv1alpha1.ValidationJob{
		{Node: testConsumerNode, JobName: testValidationJobName},
		{Node: testSiblingNode, JobName: "vmig-validate-2"},
	}
	passed := validationJob(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	pending := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "vmig-validate-2", Namespace: testVMNamespace},
	}

	r, cl := newVMReconciler(t, srv.URL, vm, passed, pending, migrationCluster())

	// First pass: one node passes, the other is still running.
	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := getVM(t, cl)
	if len(got.Status.ValidationJobs) != 2 {
		t.Fatalf("ValidationJobs = %+v, want both nodes still tracked", got.Status.ValidationJobs)
	}
	byNode := map[string]simplyblockv1alpha1.ValidationJob{}
	for _, vj := range got.Status.ValidationJobs {
		byNode[vj.Node] = vj
	}
	if !byNode[testConsumerNode].Succeeded {
		t.Errorf("%s is not recorded as passed; its Job would be run again", testConsumerNode)
	}
	if byNode[testSiblingNode].Succeeded {
		t.Errorf("%s recorded as passed while its Job is still running", testSiblingNode)
	}
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating with one node pending", got.Status.Phase)
	}

	// The passed node's Job is now reaped, as its TTL would do. A second pass must
	// treat that node as done rather than "vanished", and must not create a new Job.
	if err := cl.Delete(context.Background(), passed); err != nil {
		t.Fatalf("delete the passed job: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	got = getVM(t, cl)
	if len(got.Status.ValidationJobs) != 2 {
		t.Errorf("ValidationJobs = %+v, want both entries kept after the Job was reaped",
			got.Status.ValidationJobs)
	}
	var jobs batchv1.JobList
	if err := cl.List(context.Background(), &jobs, client.InNamespace(testVMNamespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].Name != "vmig-validate-2" {
		names := make([]string, 0, len(jobs.Items))
		for i := range jobs.Items {
			names = append(names, jobs.Items[i].Name)
		}
		t.Errorf("jobs = %v, want only the still-pending one (no recreation)", names)
	}
	if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
		t.Errorf("phase = %q, want still Validating", got.Status.Phase)
	}
}

// The pre-cutover re-check must not mistake an already-passed node for a newly
// appeared one — that was the second half of the loop, re-adding a node seconds after
// validating it.
func TestValidateLateNodes_PassedNodeIsNotTreatedAsNew(t *testing.T) {
	var continueCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			continueCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	// The consumer's node has already passed and its Job has been reaped.
	vm := validatingVM(testValidationJobName)
	vm.Status.ValidationJobs = []simplyblockv1alpha1.ValidationJob{
		{Node: testConsumerNode, JobName: testValidationJobName, Succeeded: true},
	}
	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)

	r, cl := newVMReconciler(t, srv.URL, vm, pv, pod, migrationCluster())

	if _, err := r.Reconcile(context.Background(), vmRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !continueCalled {
		t.Errorf("expected the migration to continue; the only consuming node had passed")
	}
	var jobs batchv1.JobList
	if err := cl.List(context.Background(), &jobs, client.InNamespace(testVMNamespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("created %d job(s), want none for an already-validated node", len(jobs.Items))
	}
	if got := getVM(t, cl); got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
}

// drainEvents returns the events recorded so far.
func drainEvents(t *testing.T, r *VolumeMigrationReconciler) []string {
	t.Helper()
	rec, ok := r.Recorder.(*events.FakeRecorder)
	if !ok {
		t.Fatalf("recorder is %T, not a FakeRecorder", r.Recorder)
	}
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// Every other validation test asserts what one reconcile pass does. This one asserts
// that the passes *converge*: driven repeatedly, with nodes finishing at different
// times and finished Jobs being reaped underneath, the migration must reach Running
// without ever restarting a node that already passed.
//
// That is the property the endless create/delete/recreate loop violated, and no
// single-pass test could see it — which is why it reached a cluster.
func TestReconcileValidating_ConvergesWithStaggeredCompletions(t *testing.T) {
	const siblingVolumeUUID = "sibling-vol"
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if serveSubsystemMembers(w, r, siblingVolumeUUID) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == migrationPath:
			_, _ = w.Write([]byte(`{"id":"` + testMigrationUUID + `","phase":"pre_created","status":"new"}`))
		case r.Method == http.MethodPost && r.URL.Path == continuePath:
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	pv := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+testVolumeUUID, "app-pvc")
	pod := consumerPod("app-0", testConsumerNode, "app-pvc", corev1.PodRunning)
	siblingPV := boundCSIPV(testClusterUUID+":"+testPoolUUID+":"+siblingVolumeUUID, "sib-pvc")
	siblingPV.Name = "pv-sibling"
	siblingPod := consumerPod("sib-0", testSiblingNode, "sib-pvc", corev1.PodRunning)

	r, cl := newVMReconciler(t, srv.URL, validatingVM(""), pv, pod, siblingPV, siblingPod,
		migrationCluster())

	ctx := context.Background()
	passedOnce := map[string]bool{}
	nodesSeen := map[string]bool{}
	entriesSeen := 0
	starts := 0
	reached := false

	for i := 0; i < 20 && !reached; i++ {
		if _, err := r.Reconcile(ctx, vmRequest()); err != nil {
			t.Fatalf("Reconcile (pass %d): %v", i, err)
		}
		for _, e := range drainEvents(t, r) {
			if strings.Contains(e, "ValidationStarted") {
				starts++
			}
		}

		vm := getVM(t, cl)
		if vm.Status.Phase == simplyblockv1alpha1.VolumeMigrationPhaseRunning {
			reached = true
			break
		}
		// A pass may never un-pass a node, nor lose one: dropping entries is how the
		// gate came to declare success for a subset of the consuming nodes.
		for _, vj := range vm.Status.ValidationJobs {
			nodesSeen[vj.Node] = true
			if vj.Succeeded {
				passedOnce[vj.Node] = true
			} else if passedOnce[vj.Node] {
				t.Fatalf("pass %d: node %s went from passed back to pending", i, vj.Node)
			}
		}
		if n := len(vm.Status.ValidationJobs); n < entriesSeen {
			t.Fatalf("pass %d: tracked nodes shrank from %d to %d — the gate would pass on a subset",
				i, entriesSeen, n)
		} else if n > entriesSeen {
			entriesSeen = n
		}

		// Finish one outstanding Job per pass, so the nodes complete at different
		// times — the situation the single-pass tests never produce.
		var jobs batchv1.JobList
		if err := cl.List(ctx, &jobs, client.InNamespace(testVMNamespace)); err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		for j := range jobs.Items {
			job := &jobs.Items[j]
			if len(job.Status.Conditions) == 0 {
				job.Status.Conditions = []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				}
				if err := cl.Status().Update(ctx, job); err != nil {
					t.Fatalf("complete job %s: %v", job.Name, err)
				}
				break
			}
		}
		// Reap the Jobs of nodes already recorded as passed, as their TTL would.
		for _, vj := range getVM(t, cl).Status.ValidationJobs {
			if !vj.Succeeded {
				continue
			}
			_ = cl.Delete(ctx, &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: vj.JobName, Namespace: testVMNamespace},
			})
		}
	}

	if !reached {
		vm := getVM(t, cl)
		t.Fatalf("validation never converged: phase %q, jobs %+v after 20 passes",
			vm.Status.Phase, vm.Status.ValidationJobs)
	}
	if len(nodesSeen) != 2 {
		t.Errorf("nodes tracked = %v, want both consuming nodes", nodesSeen)
	}
	if entriesSeen != 2 {
		t.Errorf("most nodes tracked at once = %d, want 2", entriesSeen)
	}
	// At least one node must have been observed passing while another was still
	// pending — otherwise the completions were not actually staggered and the test
	// would not exercise the case that broke.
	if len(passedOnce) == 0 {
		t.Errorf("no node was observed passing before the phase advanced; completions were not staggered")
	}
	// Validation is started once, for the two nodes together. More would mean a node
	// was rebuilt after having been validated.
	if starts != 1 {
		t.Errorf("validation was started %d times, want once for the whole node set", starts)
	}
}

// A create whose answer never arrived must not fail the migration: the control plane may
// have created it (and allocated bdevs for it) while the client gave up, and only a later
// attempt can observe and cancel that. Failing here abandons it.
func TestIsIndeterminateCreate(t *testing.T) {
	timeoutErr := &net.OpError{Op: "dial", Err: &timeoutError{}}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"client timeout", fmt.Errorf("http error: %w", context.DeadlineExceeded), true},
		{"context cancelled", fmt.Errorf("wrapped: %w", context.Canceled), true},
		{"net timeout", fmt.Errorf("post: %w", timeoutErr), true},
		// An HTTP status is an answer: the control plane decided, so this is a real failure.
		{"rejected by the backend", errors.New("create migration: unexpected status 400"), false},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIndeterminateCreate(tc.err); got != tc.want {
				t.Errorf("isIndeterminateCreate(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
