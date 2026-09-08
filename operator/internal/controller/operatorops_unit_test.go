// The discovery run, driven step by step against a fake client.
//
// What is worth testing here is not that the three steps run — it is that each
// one persists what it concluded, because everything after it has to agree with
// what it decided. A run that re-listed the nodes in Probing would put a worker
// that joined mid-run into the draft with no report to describe it, and a run
// that re-derived the environment in Writing could write a different one from
// the one it told the reviewer about.
//
// The probes themselves are not run. What stands in for them is the ConfigMap a
// probe writes, which is the contract between the two halves, so these tests
// exercise the reconciler against exactly what it will read in a cluster.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/blockdev"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

const (
	opsNamespace = "simplyblock"
	opsName      = "oops-1"
	opsImage     = "quay.io/simplyblock-io/simplyblock-operator:26.4.0"
)

// opsScheme is the scheme the fake client is built over.
func opsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// discoverRun is an OperatorOps asking for a discovery run.
func discoverRun(spec *simplyblockv1alpha1.DiscoverSpec) *simplyblockv1alpha1.OperatorOps {
	return &simplyblockv1alpha1.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:       opsName,
			Namespace:  opsNamespace,
			Generation: 1,
			Finalizers: []string{operatorOpsFinalizer},
		},
		Spec: simplyblockv1alpha1.OperatorOpsSpec{
			Action:   simplyblockv1alpha1.OperatorOpsActionDiscover,
			Discover: spec,
		},
	}
}

// worker is a schedulable Kubernetes node running a K3s kubelet, which is the
// fleet this was developed against.
func worker(name string, options ...func(*corev1.Node)) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			KubeletVersion: "v1.35.5+k3s1",
			OSImage:        "Rocky Linux 9.5 (Blue Onyx)",
		}},
	}
	for _, option := range options {
		option(node)
	}
	return node
}

func cordoned(node *corev1.Node) { node.Spec.Unschedulable = true }

func tainted(node *corev1.Node) {
	node.Spec.Taints = []corev1.Taint{{
		Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule,
	}}
}

func labeled(key, value string) func(*corev1.Node) {
	return func(node *corev1.Node) {
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[key] = value
	}
}

// reportConfigMap is what a probe on a worker writes: two free NVMe disks on
// memory node 0 and two on node 1, which is the shape the placement has a
// choice about.
func reportConfigMap(t *testing.T, node string, addresses ...string) *corev1.ConfigMap {
	t.Helper()

	const tb = uint64(1) << 40
	report := nodeprobe.Report{
		Version: nodeprobe.ReportVersion,
		Node:    node,
		CPU: nodeprobe.CPU{
			OnlineCPUs: 32, PhysicalCores: 16, Sockets: 2, ThreadsPerCore: 2, HyperThreading: true,
			NUMANodes: []nodeprobe.NUMACPUs{
				{Node: 0, OnlineCPUs: []int{0, 1, 2, 3}, PhysicalCores: 8},
				{Node: 1, OnlineCPUs: []int{4, 5, 6, 7}, PhysicalCores: 8},
			},
		},
		HugePages: []nodeprobe.HugePagePool{{
			SizeBytes: 1 << 30, Total: 32, Free: 32,
			NUMANodes: []nodeprobe.NUMAHugePages{
				{Node: 0, Total: 16, Free: 16},
				{Node: 1, Total: 16, Free: 16},
			},
		}},
	}
	for i, address := range addresses {
		report.Devices = append(report.Devices, nodeprobe.Device{
			Name:       fmt.Sprintf("nvme%dn1", i),
			Path:       fmt.Sprintf("/dev/nvme%dn1", i),
			PCIAddress: address,
			SizeBytes:  3 * tb,
			Kind:       string(blockdev.KindDisk),
			Transport:  string(blockdev.TransportNVMe),
			NUMANode:   i / 2,
			Available:  true,
			Content:    "Blank",
		})
	}

	cm, err := nodeprobe.ConfigMap(opsNamespace, opsName, nil, report)
	if err != nil {
		t.Fatalf("render the report ConfigMap: %v", err)
	}
	return cm
}

// runner drives a reconciler over a fake client.
type runner struct {
	t          *testing.T
	reconciler *OperatorOpsReconciler
	client     client.Client
}

// newRunner builds a reconciler over the objects given.
func newRunner(t *testing.T, objects ...client.Object) *runner {
	t.Helper()
	scheme := opsScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha1.OperatorOps{}).
		Build()

	return &runner{
		t:      t,
		client: c,
		reconciler: &OperatorOpsReconciler{
			Client:     c,
			Scheme:     scheme,
			ProbeImage: opsImage,
		},
	}
}

// step reconciles once and returns the run as it stands afterward.
func (r *runner) step() (ctrl.Result, *simplyblockv1alpha1.OperatorOps) {
	r.t.Helper()
	result, err := r.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: opsName, Namespace: opsNamespace},
	})
	if err != nil {
		r.t.Fatalf("reconcile: %v", err)
	}

	var ops simplyblockv1alpha1.OperatorOps
	if err := r.client.Get(context.Background(),
		types.NamespacedName{Name: opsName, Namespace: opsNamespace}, &ops); err != nil {
		r.t.Fatalf("read the run back: %v", err)
	}
	return result, &ops
}

// jobs is the probe Jobs that exist.
func (r *runner) jobs() []batchv1.Job {
	r.t.Helper()
	var list batchv1.JobList
	if err := r.client.List(context.Background(), &list, client.InNamespace(opsNamespace)); err != nil {
		r.t.Fatalf("list the Jobs: %v", err)
	}
	return list.Items
}

// configs is the ClusterDeploymentConfigs that exist.
func (r *runner) configs() []simplyblockv1alpha1.ClusterDeploymentConfig {
	r.t.Helper()
	var list simplyblockv1alpha1.ClusterDeploymentConfigList
	if err := r.client.List(context.Background(), &list, client.InNamespace(opsNamespace)); err != nil {
		r.t.Fatalf("list the configs: %v", err)
	}
	return list.Items
}

func TestDiscoverInspectingSettlesTheWorkersAndTheEnvironment(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("worker-1"), worker("worker-2"))

	_, ops := r.step() // start
	if ops.Status.Phase != simplyblockv1alpha1.OperatorOpsPhaseRunning {
		t.Fatalf("the run is %q after starting, want Running", ops.Status.Phase)
	}
	if ops.Status.Step.State != string(simplyblockv1alpha1.OperatorOpsStepInspecting) {
		t.Fatalf("the run is on step %q, want Inspecting", ops.Status.Step.State)
	}
	if ops.Status.StartedAt == nil {
		t.Error("a started run does not say when it started")
	}

	_, ops = r.step() // inspect
	if !slices.Equal(ops.Status.Workers, []string{"worker-1", "worker-2"}) {
		t.Errorf("the run settled on %v", ops.Status.Workers)
	}
	if ops.Status.Environment != simplyblockv1alpha1.KubernetesEnvironmentK3s {
		t.Errorf("concluded the environment %q, want K3s from the kubelet version",
			ops.Status.Environment)
	}
	if ops.Status.Step.State != string(simplyblockv1alpha1.OperatorOpsStepProbing) {
		t.Errorf("the run is on step %q, want Probing", ops.Status.Step.State)
	}
	if _, has := ops.Status.Step.KubeDeadline(); !has {
		t.Error("Probing has no deadline, so a Job that never finishes holds the run open")
	}
	if ops.Status.ObservedGeneration != 1 {
		t.Errorf("the status was computed from generation %d", ops.Status.ObservedGeneration)
	}
}

func TestDiscoverSkipsWorkersNothingCanBePlacedOn(t *testing.T) {
	// A cordoned node and a tainted one are both excluded: a probe Job is
	// pinned with spec.nodeName and would run on either, and a worker the
	// cluster is not scheduling to is not one to hand to a storage cluster.
	r := newRunner(t,
		discoverRun(nil),
		worker("worker-1"),
		worker("worker-cordoned", cordoned),
		worker("control-plane", tainted),
	)

	r.step()
	_, ops := r.step()

	if !slices.Equal(ops.Status.Workers, []string{"worker-1"}) {
		t.Errorf("the run settled on %v, want worker-1 alone", ops.Status.Workers)
	}
}

func TestDiscoverSkipsWorkersAStorageNodeAlreadyRunsOn(t *testing.T) {
	// A run reports only what is unclaimed, which is what makes re-running it
	// useful: a run against a deployed fleet finds the machines nobody has
	// taken yet.
	taken := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-1", Namespace: opsNamespace},
		Spec:       simplyblockv1alpha1.StorageNodeSpec{WorkerNode: "worker-1"},
	}
	r := newRunner(t, discoverRun(nil), worker("worker-1"), worker("worker-2"), taken)

	r.step()
	_, ops := r.step()

	if !slices.Equal(ops.Status.Workers, []string{"worker-2"}) {
		t.Errorf("the run settled on %v, want the untaken worker alone", ops.Status.Workers)
	}
}

func TestDiscoverHonorsTheNodeSelector(t *testing.T) {
	r := newRunner(t,
		discoverRun(&simplyblockv1alpha1.DiscoverSpec{
			NodeSelector: map[string]string{"simplyblock.io/storage": "true"},
		}),
		worker("worker-1", labeled("simplyblock.io/storage", "true")),
		worker("worker-2"),
	)

	r.step()
	_, ops := r.step()

	if !slices.Equal(ops.Status.Workers, []string{"worker-1"}) {
		t.Errorf("the run settled on %v, want the selected worker alone", ops.Status.Workers)
	}
}

func TestDiscoverFailsWhenNoWorkerIsFree(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("control-plane", tainted))

	r.step()
	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha1.OperatorOpsPhaseFailed {
		t.Fatalf("the run is %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "no schedulable worker") {
		t.Errorf("the message is %q, and it does not say what was wrong", ops.Status.Message)
	}
}

func TestDiscoverProbingCreatesOneJobPerWorkerAndOnlyOnce(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("worker-1"), worker("worker-2"))

	r.step() // start
	r.step() // inspect
	result, ops := r.step()

	jobs := r.jobs()
	if len(jobs) != 2 {
		t.Fatalf("created %d Jobs for 2 workers", len(jobs))
	}
	pinned := map[string]bool{}
	for _, job := range jobs {
		pinned[job.Spec.Template.Spec.NodeName] = true
		if job.Spec.Template.Spec.Containers[0].Image != opsImage {
			t.Errorf("a Job runs %q, want the operator's own image",
				job.Spec.Template.Spec.Containers[0].Image)
		}
		if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Name != opsName {
			t.Errorf("a Job is owned by %+v, want the run", job.OwnerReferences)
		}
	}
	if !pinned["worker-1"] || !pinned["worker-2"] {
		t.Errorf("the Jobs are pinned to %v", pinned)
	}
	if result.RequeueAfter == 0 {
		t.Error("Probing did not ask to be looked at again while its Jobs run")
	}
	if ops.Status.Step.State != string(simplyblockv1alpha1.OperatorOpsStepProbing) {
		t.Errorf("the run left Probing before any report arrived")
	}

	// Reconciling again must find the Jobs rather than create a second pair:
	// the name is derived from the run and the worker, which is what makes the
	// step idempotent.
	r.step()
	if again := r.jobs(); len(again) != 2 {
		t.Errorf("a second reconcile brought the Job count to %d", len(again))
	}
}

func TestDiscoverWritesADraftFromTheReports(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("worker-1"), worker("worker-2"))

	r.step() // start
	r.step() // inspect
	r.step() // probing: creates the Jobs

	// The probes finish and write their reports.
	for _, node := range []string{"worker-1", "worker-2"} {
		cm := reportConfigMap(t, node,
			"0000:5e:00.0", "0000:5f:00.0", "0000:af:00.0", "0000:b0:00.0")
		if err := r.client.Create(context.Background(), cm); err != nil {
			t.Fatalf("write a report: %v", err)
		}
	}

	_, ops := r.step() // probing: sees the reports, moves to Writing
	if ops.Status.Step.State != string(simplyblockv1alpha1.OperatorOpsStepWriting) {
		t.Fatalf("the run is on step %q, want Writing", ops.Status.Step.State)
	}

	_, ops = r.step() // writing
	if ops.Status.Phase != simplyblockv1alpha1.OperatorOpsPhaseSucceeded {
		t.Fatalf("the run is %q: %s", ops.Status.Phase, ops.Status.Message)
	}
	if ops.Status.CompletedAt == nil {
		t.Error("a finished run does not say when it finished")
	}

	configs := r.configs()
	if len(configs) != 1 {
		t.Fatalf("wrote %d documents", len(configs))
	}
	config := configs[0]
	if config.Name != ops.Status.ConfigRef {
		t.Errorf("the run points at %q and the document is %q", ops.Status.ConfigRef, config.Name)
	}

	// Always a draft, and never anything else.
	if config.Spec.Approved {
		t.Error("the document was written approved")
	}
	if config.Spec.Environment != simplyblockv1alpha1.KubernetesEnvironmentK3s {
		t.Errorf("the document says environment %q, want the one Inspecting concluded",
			config.Spec.Environment)
	}

	if len(config.Spec.NodeSets) != 1 {
		t.Fatalf("the document has %d node sets, want 1", len(config.Spec.NodeSets))
	}
	set := config.Spec.NodeSets[0]
	if len(set.Groups) != 1 {
		t.Fatalf("the node set has %d groups, want 1 for an identical fleet: %+v", len(set.Groups), set.Groups)
	}
	group := set.Groups[0]
	if !slices.Equal(group.Workers, []string{"worker-1", "worker-2"}) {
		t.Errorf("the group holds %v", group.Workers)
	}
	// The placement chose one memory node, so two of the four disks.
	if group.Devices == nil || len(group.Devices.NVMe) != 2 {
		t.Errorf("the group names %+v, want the two disks on the chosen node", group.Devices)
	}

	// The cluster the draft proposes has the two fields the API requires and
	// no worker reports.
	if config.Spec.Cluster == nil {
		t.Fatal("the document proposes no cluster")
	}
	if config.Spec.Cluster.VCPUCount == nil || *config.Spec.Cluster.VCPUCount != 8 {
		t.Errorf("the cluster asks for %v vCPUs, want the 8 cores of the chosen node",
			config.Spec.Cluster.VCPUCount)
	}
	if config.Spec.Cluster.MaxSubsystemCount == nil {
		t.Error("the cluster names no maxSubsystemCount, which the API requires")
	}
}

func TestDiscoverGrowsAnExistingClusterWhenToldTo(t *testing.T) {
	// A growth document names the cluster and proposes no layout: the cluster's
	// is settled, and naming a template beside a reference is what admission
	// refuses.
	r := newRunner(t,
		discoverRun(&simplyblockv1alpha1.DiscoverSpec{ClusterRef: "sb-cluster"}),
		worker("worker-1"),
	)

	r.step()
	r.step()
	r.step()
	cm := reportConfigMap(t, "worker-1", "0000:5e:00.0", "0000:5f:00.0")
	if err := r.client.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	r.step()
	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha1.OperatorOpsPhaseSucceeded {
		t.Fatalf("the run is %q: %s", ops.Status.Phase, ops.Status.Message)
	}
	config := r.configs()[0]
	if config.Spec.ClusterRef != "sb-cluster" {
		t.Errorf("the document names cluster %q", config.Spec.ClusterRef)
	}
	if config.Spec.Cluster != nil {
		t.Errorf("a growth document proposes a cluster layout as well: %+v", config.Spec.Cluster)
	}
}

func TestDiscoverNamesTheDocumentItWasAskedFor(t *testing.T) {
	r := newRunner(t,
		discoverRun(&simplyblockv1alpha1.DiscoverSpec{ConfigName: "rack-b-draft"}),
		worker("worker-1"),
	)

	r.step()
	r.step()
	r.step()
	if err := r.client.Create(context.Background(),
		reportConfigMap(t, "worker-1", "0000:5e:00.0")); err != nil {
		t.Fatal(err)
	}
	r.step()
	_, ops := r.step()

	if ops.Status.ConfigRef != "rack-b-draft" {
		t.Errorf("wrote %q, want the name the run asked for", ops.Status.ConfigRef)
	}
}

func TestDiscoverIgnoresAReportForAWorkerItIsNotAbout(t *testing.T) {
	// A reused run name, or a report left by an earlier run. It is not this
	// run's evidence, and a draft built from it would describe a machine the
	// run never inspected.
	r := newRunner(t, discoverRun(nil), worker("worker-1"))

	r.step()
	r.step()
	r.step()
	for _, node := range []string{"worker-1", "worker-stranger"} {
		if err := r.client.Create(context.Background(),
			reportConfigMap(t, node, "0000:5e:00.0", "0000:5f:00.0")); err != nil {
			t.Fatal(err)
		}
	}
	r.step()
	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha1.OperatorOpsPhaseSucceeded {
		t.Fatalf("the run is %q: %s", ops.Status.Phase, ops.Status.Message)
	}
	workers := r.configs()[0].Spec.NodeSets[0].Groups[0].Workers
	if !slices.Equal(workers, []string{"worker-1"}) {
		t.Errorf("the document holds %v, want only the worker the run inspected", workers)
	}
}

func TestDiscoverFailsWithoutAProbeImage(t *testing.T) {
	// An operator whose chart did not pass the image is fine until somebody
	// asks it to discover, and the run is where that is said.
	r := newRunner(t, discoverRun(nil), worker("worker-1"))
	r.reconciler.ProbeImage = ""

	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha1.OperatorOpsPhaseFailed {
		t.Fatalf("the run is %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, NodeProbeImageEnv) {
		t.Errorf("the message is %q, and it does not name the variable to set", ops.Status.Message)
	}
}

func TestDiscoverStopsWhenAborted(t *testing.T) {
	run := discoverRun(nil)
	run.Spec.Abort = true
	r := newRunner(t, run, worker("worker-1"))

	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha1.OperatorOpsPhaseAborted {
		t.Fatalf("the run is %q, want Aborted", ops.Status.Phase)
	}
	if len(r.jobs()) != 0 {
		t.Error("an aborted run created probe Jobs")
	}
}

func TestDiscoverIsTerminalOnceItFinishes(t *testing.T) {
	// A finished run is the audit record. Reconciling it again must change
	// nothing, or a run would rewrite its document every time the controller
	// restarted.
	r := newRunner(t, discoverRun(nil), worker("worker-1"))

	r.step()
	r.step()
	r.step()
	if err := r.client.Create(context.Background(),
		reportConfigMap(t, "worker-1", "0000:5e:00.0", "0000:5f:00.0")); err != nil {
		t.Fatal(err)
	}
	r.step()
	_, ops := r.step()
	if ops.Status.Phase != simplyblockv1alpha1.OperatorOpsPhaseSucceeded {
		t.Fatalf("the run is %q: %s", ops.Status.Phase, ops.Status.Message)
	}
	firstConfig := ops.Status.ConfigRef

	result, again := r.step()
	if again.Status.ConfigRef != firstConfig {
		t.Errorf("a reconcile of a finished run changed its document to %q", again.Status.ConfigRef)
	}
	if !result.IsZero() {
		t.Errorf("a finished run asked to be looked at again: %+v", result)
	}
	if len(r.configs()) != 1 {
		t.Errorf("a reconcile of a finished run left %d documents", len(r.configs()))
	}
}

func TestDiscoverAddsItsFinalizerBeforeDoingAnything(t *testing.T) {
	// The finalizer is what holds a run in Terminating until it is terminal, so
	// a delete arriving mid-Probing cannot leave Jobs running with nothing
	// recording them.
	run := discoverRun(nil)
	run.Finalizers = nil
	r := newRunner(t, run, worker("worker-1"))

	_, ops := r.step()

	if !slices.Contains(ops.Finalizers, operatorOpsFinalizer) {
		t.Fatalf("the run carries %v", ops.Finalizers)
	}
	if ops.Status.Phase != "" {
		t.Error("the run started before its finalizer was recorded")
	}
}
