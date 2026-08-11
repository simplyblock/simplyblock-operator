package controller

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
