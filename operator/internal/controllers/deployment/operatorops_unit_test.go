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

package deployment

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	discoverypkg "github.com/simplyblock/simplyblock-operator/internal/discovery"
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
	if err := simplyblockv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// discoverRun is an OperatorOps asking for a discovery run.
func discoverRun(spec *simplyblockv1alpha2.DiscoverSpec) *simplyblockv1alpha2.OperatorOps {
	return &simplyblockv1alpha2.OperatorOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:       opsName,
			Namespace:  opsNamespace,
			Generation: 1,
			Finalizers: []string{operatorOpsFinalizer},
		},
		Spec: simplyblockv1alpha2.OperatorOpsSpec{
			Action:   simplyblockv1alpha2.OperatorOpsActionDiscover,
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
	return newRunnerWithInterceptors(t, interceptor.Funcs{}, objects...)
}

// newRunnerWithInterceptors is the same runner with the client's answers
// scripted, which is how a test reaches the failures a real API server produces
// and a fake one never does.
func newRunnerWithInterceptors(
	t *testing.T, funcs interceptor.Funcs, objects ...client.Object,
) *runner {
	t.Helper()
	scheme := opsScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.OperatorOps{}).
		WithInterceptorFuncs(funcs).
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
func (r *runner) step() (ctrl.Result, *simplyblockv1alpha2.OperatorOps) {
	r.t.Helper()
	result, err := r.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: opsName, Namespace: opsNamespace},
	})
	if err != nil {
		r.t.Fatalf("reconcile: %v", err)
	}

	var ops simplyblockv1alpha2.OperatorOps
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
func (r *runner) configs() []simplyblockv1alpha2.ClusterDeploymentConfig {
	r.t.Helper()
	var list simplyblockv1alpha2.ClusterDeploymentConfigList
	if err := r.client.List(context.Background(), &list, client.InNamespace(opsNamespace)); err != nil {
		r.t.Fatalf("list the configs: %v", err)
	}
	return list.Items
}

func TestDiscoverInspectingSettlesTheWorkersAndTheEnvironment(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("worker-1"), worker("worker-2"))

	_, ops := r.step() // start
	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseRunning {
		t.Fatalf("the run is %q after starting, want Running", ops.Status.Phase)
	}
	if ops.Status.Step.State != string(simplyblockv1alpha2.OperatorOpsStepInspecting) {
		t.Fatalf("the run is on step %q, want Inspecting", ops.Status.Step.State)
	}
	if ops.Status.StartedAt == nil {
		t.Error("a started run does not say when it started")
	}

	_, ops = r.step() // inspect
	if !slices.Equal(ops.Status.Workers, []string{"worker-1", "worker-2"}) {
		t.Errorf("the run settled on %v", ops.Status.Workers)
	}
	if ops.Status.Environment != simplyblockv1alpha2.KubernetesEnvironmentK3s {
		t.Errorf("concluded the environment %q, want K3s from the kubelet version",
			ops.Status.Environment)
	}
	if ops.Status.Step.State != string(simplyblockv1alpha2.OperatorOpsStepProbing) {
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
	taken := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sn-1", Namespace: opsNamespace},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{WorkerNode: "worker-1"},
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
		discoverRun(&simplyblockv1alpha2.DiscoverSpec{
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

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseFailed {
		t.Fatalf("the run is %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "no worker is free") {
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
	if ops.Status.Step.State != string(simplyblockv1alpha2.OperatorOpsStepProbing) {
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
	if ops.Status.Step.State != string(simplyblockv1alpha2.OperatorOpsStepWriting) {
		t.Fatalf("the run is on step %q, want Writing", ops.Status.Step.State)
	}

	_, ops = r.step() // writing
	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseSucceeded {
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
	if config.Spec.Environment != simplyblockv1alpha2.KubernetesEnvironmentK3s {
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
		discoverRun(&simplyblockv1alpha2.DiscoverSpec{ClusterRef: "sb-cluster"}),
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

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseSucceeded {
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
		discoverRun(&simplyblockv1alpha2.DiscoverSpec{ConfigName: "rack-b-draft"}),
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

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseSucceeded {
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

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseFailed {
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

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseAborted {
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
	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseSucceeded {
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

// heldReportConfigMap is a machine whose NVMe controllers are on a userspace
// driver. The kernel presents no disk for such a controller, so the report
// carries no device at all and the controllers say where the disks went.
//
// Something is driving them, which is what puts the machine out of reach: a
// binding nothing is using is a disk the draft claims, and only a held one is a
// disk in service. Whatever holds it need not be this product.
func heldReportConfigMap(t *testing.T, node string, addresses ...string) *corev1.ConfigMap {
	t.Helper()

	report := nodeprobe.Report{
		Version: nodeprobe.ReportVersion,
		Node:    node,
		CPU: nodeprobe.CPU{
			OnlineCPUs: 12, PhysicalCores: 12, Sockets: 1, ThreadsPerCore: 1,
			NUMANodes: []nodeprobe.NUMACPUs{{Node: 0, OnlineCPUs: []int{0, 1, 2, 3}, PhysicalCores: 12}},
		},
		HugePages: []nodeprobe.HugePagePool{{
			SizeBytes: 1 << 21, Total: 3584, Free: 3584,
			NUMANodes: []nodeprobe.NUMAHugePages{{Node: 0, Total: 3584, Free: 3584}},
		}},
	}
	for _, address := range addresses {
		report.NVMeControllers = append(report.NVMeControllers, nodeprobe.Controller{
			Address:  address,
			Driver:   "uio_pci_generic",
			NUMANode: -1,
			// Held rather than idle, which is what makes the machine unusable:
			// an idle binding is a disk the draft now claims.
			InUse: ptr.To(true),
		})
	}

	cm, err := nodeprobe.ConfigMap(opsNamespace, opsName, nil, report)
	if err != nil {
		t.Fatalf("render the report ConfigMap: %v", err)
	}
	return cm
}

// A run that produces nothing owes the reason it produced nothing.
//
// The rules compute one: a worker whose controllers are held by a userspace
// driver is refused with the controllers and the driver named, which is the
// difference between a reviewer concluding the machines have no storage and
// knowing to reclaim them. That explanation was computed and then dropped,
// because the failure returned before anything reported a refusal, and what the
// reviewer was left with was a count.
func TestARunThatFindsNothingSaysWhy(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("worker-1"), worker("worker-2"))

	r.step() // start
	r.step() // inspect
	r.step() // probing: creates the Jobs

	for _, node := range []string{"worker-1", "worker-2"} {
		cm := heldReportConfigMap(t, node,
			"0000:00:02.0", "0000:00:03.0", "0000:00:04.0", "0000:00:05.0")
		if err := r.client.Create(context.Background(), cm); err != nil {
			t.Fatalf("write a report: %v", err)
		}
	}

	r.step() // probing: sees the reports, moves to Writing
	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseFailed {
		t.Fatalf("the run is %q, want Failed: %s", ops.Status.Phase, ops.Status.Message)
	}
	if len(r.configs()) != 0 {
		t.Error("a run with no usable device wrote a document anyway")
	}

	// What the message has to carry is the reason, not the arithmetic of it.
	for _, want := range []string{"uio_pci_generic", "0000:00:02.0", "worker-1"} {
		if !strings.Contains(ops.Status.Message, want) {
			t.Errorf("the failure does not mention %q: %s", want, ops.Status.Message)
		}
	}
}

// partitionedReportConfigMap is a machine whose NVMe disks are on the kernel
// driver, whole, of the right class, and carrying a partition table somebody
// left on them.
//
// It is the other half of a lab that ran simplyblock before: the controllers
// were handed back to the kernel and the disks still hold the old table.
func partitionedReportConfigMap(t *testing.T, node string, addresses ...string) *corev1.ConfigMap {
	t.Helper()

	const gb = uint64(1) << 30
	report := nodeprobe.Report{
		Version: nodeprobe.ReportVersion,
		Node:    node,
		CPU: nodeprobe.CPU{
			OnlineCPUs: 12, PhysicalCores: 12, Sockets: 1, ThreadsPerCore: 1,
			NUMANodes: []nodeprobe.NUMACPUs{{Node: 0, OnlineCPUs: []int{0, 1, 2, 3}, PhysicalCores: 12}},
		},
		HugePages: []nodeprobe.HugePagePool{{
			SizeBytes: 1 << 21, Total: 3584, Free: 3584,
			NUMANodes: []nodeprobe.NUMAHugePages{{Node: 0, Total: 3584, Free: 3584}},
		}},
	}
	for i, address := range addresses {
		report.NVMeControllers = append(report.NVMeControllers, nodeprobe.Controller{
			Address: address, Driver: "nvme", NUMANode: -1,
		})
		report.Devices = append(report.Devices, nodeprobe.Device{
			Name:       fmt.Sprintf("nvme%dn1", i),
			Path:       fmt.Sprintf("/dev/nvme%dn1", i),
			PCIAddress: address,
			SizeBytes:  70 * gb,
			Kind:       string(blockdev.KindDisk),
			Transport:  string(blockdev.TransportNVMe),
			NUMANode:   -1,
			Available:  false,
			Content:    "Foreign",
			Rejections: []nodeprobe.Rejection{{
				Reason: string(blockdev.ReasonPartitioned),
				Detail: "GPT header at 4096 (LBA 1), MBR partition table at 446",
			}},
		})
		// The machine also presents the devices nothing would ever take, which
		// is what makes the reporting hard: they outnumber the disks that
		// matter and they are refused for reasons nobody needs.
		report.Devices = append(report.Devices, nodeprobe.Device{
			Name: fmt.Sprintf("nbd%d", i), Path: fmt.Sprintf("/dev/nbd%d", i),
			Kind: string(blockdev.KindNetwork), NUMANode: -1,
		})
	}

	cm, err := nodeprobe.ConfigMap(opsNamespace, opsName, nil, report)
	if err != nil {
		t.Fatalf("render the report ConfigMap: %v", err)
	}
	return cm
}

// A worker refused one device at a time still owes the reason.
//
// Its worker-level refusal reads "no device of it survived the device rules."
// That is the arithmetic and not the reason. The reason is on the devices, and
// it has to reach the message past the devices refused for being loopback or
// network block devices, which outnumber it and explain nothing.
func TestARunRefusedDeviceByDeviceSaysWhichReasonMatters(t *testing.T) {
	r := newRunner(t, discoverRun(nil), worker("worker-1"))

	r.step() // start
	r.step() // inspect
	r.step() // probing: creates the Jobs

	cm := partitionedReportConfigMap(t, "worker-1",
		"0000:00:02.0", "0000:00:03.0", "0000:00:04.0", "0000:00:05.0")
	if err := r.client.Create(context.Background(), cm); err != nil {
		t.Fatalf("write a report: %v", err)
	}

	r.step() // probing: sees the report, moves to Writing
	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseFailed {
		t.Fatalf("the run is %q, want Failed: %s", ops.Status.Phase, ops.Status.Message)
	}
	if !strings.Contains(ops.Status.Message, string(blockdev.ReasonPartitioned)) {
		t.Errorf("the failure does not say the disks are partitioned: %s", ops.Status.Message)
	}
	// The devices that were never candidates are noise, and a message they
	// reach is one nobody finishes reading.
	if strings.Contains(ops.Status.Message, "whole disk") {
		t.Errorf("the failure reports devices that were never candidates: %s", ops.Status.Message)
	}
}

// A step that outlives its deadline says what outliving it means for this run,
// not only that it happened. Probing's partial reports survive in ConfigMaps
// labeled for the run, and a failure message that did not say so would leave a
// reviewer with the evidence in the cluster and no way to know it is there.
func TestARunThatOutlivesItsDeadlineSaysWhereTheEvidenceIs(t *testing.T) {
	ops := discoverRun(nil)
	ops.Status.Phase = simplyblockv1alpha2.OperatorOpsPhaseRunning
	ops.Status.Workers = []string{"worker-1"}
	ops.Status.Step.State = string(simplyblockv1alpha2.OperatorOpsStepProbing)
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	ops.Status.Step.Deadline = &expired

	r := newRunner(t, ops, worker("worker-1"))
	_, got := r.step()

	if got.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed on a step past its deadline", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "ConfigMaps") {
		t.Errorf("the failure does not say where the reports that did arrive are: %q",
			got.Status.Message)
	}
}

// The draft states the operating system the probes read, so that the reviewer
// approving it sees what the cluster will be told and the expansion has
// something to spend on ubuntuHost.
func TestTheDraftStatesTheHostOSTheProbesRead(t *testing.T) {
	r := &OperatorOpsReconciler{}
	ops := &simplyblockv1alpha2.OperatorOps{ObjectMeta: metav1.ObjectMeta{Name: "discover-1"}}
	plan := discoverypkg.Plan{Workers: []discoverypkg.Worker{{
		Name: "worker-01",
		Report: nodeprobe.Report{Node: "worker-01", HostOS: nodeprobe.HostOS{
			Distro: "ubuntu", Family: "Debian", Version: "22.04",
		}},
	}}}

	config, notes := r.draftFor(ops, &simplyblockv1alpha2.DiscoverSpec{}, plan)

	if config.Spec.HostOS == nil {
		t.Fatalf("the draft states no host OS; the notes are %v", notes)
	}
	if config.Spec.HostOS.Distro != simplyblockv1alpha2.DistroUbuntu {
		t.Errorf("the draft states the distro %q, want %q",
			config.Spec.HostOS.Distro, simplyblockv1alpha2.DistroUbuntu)
	}
	if config.Spec.HostOS.Family != simplyblockv1alpha2.HostOSFamilyDebian {
		t.Errorf("the draft states the family %q, want %q",
			config.Spec.HostOS.Family, simplyblockv1alpha2.HostOSFamilyDebian)
	}
	if !slices.ContainsFunc(notes, func(note string) bool { return strings.Contains(note, "hostOS") }) {
		t.Errorf("the notes are %v, and none of them explains the host OS", notes)
	}
}

// A fleet whose workers run different distributions gets no host OS and a note
// naming the split, because one document becomes one DaemonSet with one flag.
func TestTheDraftStatesNoHostOSForAFleetThatDisagrees(t *testing.T) {
	r := &OperatorOpsReconciler{}
	ops := &simplyblockv1alpha2.OperatorOps{ObjectMeta: metav1.ObjectMeta{Name: "discover-1"}}
	worker := func(name, distro string) discoverypkg.Worker {
		return discoverypkg.Worker{
			Name:   name,
			Report: nodeprobe.Report{Node: name, HostOS: nodeprobe.HostOS{Distro: distro}},
		}
	}
	plan := discoverypkg.Plan{Workers: []discoverypkg.Worker{
		worker("worker-01", "ubuntu"),
		worker("worker-02", "rocky"),
	}}

	config, notes := r.draftFor(ops, &simplyblockv1alpha2.DiscoverSpec{}, plan)

	if config.Spec.HostOS != nil {
		t.Fatalf("the draft states %+v for a fleet running two distributions", config.Spec.HostOS)
	}
	if !slices.ContainsFunc(notes, func(note string) bool { return strings.Contains(note, "worker-02") }) {
		t.Errorf("the notes are %v, and none of them says which worker runs what", notes)
	}
}

// storagePlaneTaint is what a fleet that dedicates machines to storage puts on
// them, and what a probe and a storage node both have to tolerate to land
// there.
var storagePlaneTaint = []corev1.Toleration{{
	Key:      "io.simplyblock.node-type",
	Operator: corev1.TolerationOpEqual,
	Value:    "storage-plane",
	Effect:   corev1.TaintEffectNoSchedule,
}}

// A probe pod is pinned to its worker rather than scheduled onto it, but a
// taint still evicts what the scheduler was bypassed for. A run against a
// tainted fleet that tolerated nothing probed nothing.
func TestDiscoverProbesTolerateWhatTheRunWasToldTo(t *testing.T) {
	run := discoverRun(&simplyblockv1alpha2.DiscoverSpec{Tolerations: storagePlaneTaint})
	r := newRunner(t, run, worker("worker-1"))

	r.step() // start
	r.step() // inspect
	r.step() // probe

	jobs := r.jobs()
	if len(jobs) != 1 {
		t.Fatalf("created %d Jobs for 1 worker", len(jobs))
	}
	if !reflect.DeepEqual(jobs[0].Spec.Template.Spec.Tolerations, storagePlaneTaint) {
		t.Errorf("the probe tolerates %+v, want %+v",
			jobs[0].Spec.Template.Spec.Tolerations, storagePlaneTaint)
	}
}

// The taints a run was allowed to probe through are the taints the cluster it
// drafts has to live with, so the draft states them rather than leaving a
// reviewer to work out that the DaemonSet will schedule nowhere.
func TestTheDraftCarriesTheTolerationsTheRunProbedWith(t *testing.T) {
	r := &OperatorOpsReconciler{}
	ops := &simplyblockv1alpha2.OperatorOps{ObjectMeta: metav1.ObjectMeta{Name: "discover-1"}}
	spec := &simplyblockv1alpha2.DiscoverSpec{Tolerations: storagePlaneTaint}

	config, notes := r.draftFor(ops, spec, discoverypkg.Plan{})

	if config.Spec.Cluster == nil {
		t.Fatalf("the draft describes no cluster; the notes are %v", notes)
	}
	if !reflect.DeepEqual(config.Spec.Cluster.Tolerations, storagePlaneTaint) {
		t.Errorf("the draft tolerates %+v, want %+v", config.Spec.Cluster.Tolerations, storagePlaneTaint)
	}
	if !slices.ContainsFunc(notes, func(note string) bool { return strings.Contains(note, "tolerations") }) {
		t.Errorf("the notes are %v, and none of them says where the tolerations came from", notes)
	}
}

// A growth document names a cluster that already states what it tolerates, so
// there is no template to put them on and nothing to restate.
func TestAGrowthDraftCarriesNoTolerations(t *testing.T) {
	r := &OperatorOpsReconciler{}
	ops := &simplyblockv1alpha2.OperatorOps{ObjectMeta: metav1.ObjectMeta{Name: "discover-1"}}
	spec := &simplyblockv1alpha2.DiscoverSpec{
		ClusterRef:  "an-existing-cluster",
		Tolerations: storagePlaneTaint,
	}

	config, _ := r.draftFor(ops, spec, discoverypkg.Plan{})

	if config.Spec.Cluster != nil {
		t.Errorf("a growth draft describes a cluster: %+v", config.Spec.Cluster)
	}
}
