// What the expansion does, and the two things it refuses to do.
//
// The cases that matter most are §6's: a config creates or adds and never
// reconciles a difference, because the differences that matter are of the form:
// this node's device list changed, and its only correct handling is not to apply
// it to a node that already has data on those devices. The other is the
// idempotence CreatingNodes needs, since a crash part-way through must create the
// rest rather than a second copy of everything.

package deployment

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

const (
	theNamespace = "simplyblock"
	theCluster   = "production"
)

// aDocument is a valid, approved two-worker NVMe deployment, so a case states only
// what it is about.
func aDocument(
	mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig),
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	config := &simplyblockv1alpha2.ClusterDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "deployment", Namespace: theNamespace},
		Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
			Approved:    true,
			Environment: simplyblockv1alpha2.KubernetesEnvironmentVanilla,
			Cluster: &simplyblockv1alpha2.ClusterTemplate{
				Name:              theCluster,
				MaxSubsystemCount: ptr.To(int32(20)),
				VCPUCount:         ptr.To(int32(8)),
				MinHugePagesSize:  "100G",
			},
			NodeSets: []simplyblockv1alpha2.NodeSet{{
				Name: "rack-a",
				Groups: []simplyblockv1alpha2.NodeGroup{{
					Name:           "saturn",
					Workers:        []string{"worker-1", "worker-2"},
					MgmtInterface:  "eth1",
					DataInterfaces: []string{"eth2"},
					Devices: &simplyblockv1alpha2.DeviceSelection{
						NVMe: []string{"0000:5e:00.0"},
					},
				}},
			}},
		},
	}
	if mutate != nil {
		mutate(config)
	}
	return config
}

// aCluster is a StorageCluster the control plane has already created.
func aCluster(
	mutate func(*simplyblockv1alpha2.StorageCluster),
) *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: theCluster, Namespace: theNamespace},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			VCPUCount:        ptr.To(int32(8)),
			MinHugePagesSize: "100G",
		},
		Status: simplyblockv1alpha2.StorageClusterStatus{UUID: "cluster-uuid"},
	}
	if mutate != nil {
		mutate(cluster)
	}
	return cluster
}

func workers(names ...string) []client.Object {
	out := make([]client.Object, 0, len(names))
	for _, name := range names {
		out = append(out, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	return out
}

func reconcilerFor(t *testing.T, objects ...client.Object) *ClusterDeploymentConfigReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)
	builder := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha2.ClusterDeploymentConfig{}).
		WithObjects(objects...)
	return &ClusterDeploymentConfigReconciler{
		Client:    builder.Build(),
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(64),
		Namespace: theNamespace,
	}
}

// A config that names a cluster it did not ask to create is refused rather than
// merged. Merging would have the operator decide what a difference means (§6).
func TestCreatingAClusterThatAlreadyExistsIsRefused(t *testing.T) {
	config := aDocument(nil)
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	_, err := r.createCluster(context.Background(), config)
	if err == nil {
		t.Fatal("a document creating an existing cluster was accepted")
	}
	if !strings.Contains(err.Error(), "spec.clusterRef") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}
}

// The mirror image: a growth document whose cluster is not there.
func TestGrowingAClusterThatDoesNotExistIsRefused(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.ClusterRef = "absent"
		c.Spec.Cluster = nil
	})
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	_, err := r.createCluster(context.Background(), config)
	if err == nil {
		t.Fatal("a document growing a cluster that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the refusal does not say the cluster is missing: %v", err)
	}
}

// The ordinary path: the cluster is created, and the class is stamped off the
// groups because the document carries no field for it.
func TestTheClusterIsCreatedWithTheClassReadOffTheGroups(t *testing.T) {
	config := aDocument(nil)
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	done, err := r.createCluster(context.Background(), config)
	if err != nil || !done {
		t.Fatalf("createCluster: done=%v err=%v", done, err)
	}

	var created simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: theNamespace, Name: theCluster}
	if err := r.Get(context.Background(), key, &created); err != nil {
		t.Fatalf("the cluster was not created: %v", err)
	}
	if created.Spec.DeviceClass != simplyblockv1alpha2.StorageClusterDeviceClassNVMe {
		t.Errorf("deviceClass = %q, want NVMe read off the groups", created.Spec.DeviceClass)
	}
	if created.Spec.StorageNodes == nil || created.Spec.StorageNodes.MgmtInterface != "eth1" {
		t.Errorf("the workload did not take the group's interfaces: %+v", created.Spec.StorageNodes)
	}
}

// One node per worker per slot, with the cluster's sizing copied in and the
// group's devices expanded into the one list a node carries.
func TestTheNodesAreCreatedFromTheGroups(t *testing.T) {
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	done, err := r.createNodes(context.Background(), config)
	if err != nil || !done {
		t.Fatalf("createNodes: done=%v err=%v", done, err)
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing the nodes: %v", err)
	}
	if len(nodes.Items) != 2 {
		t.Fatalf("created %d nodes, want one per worker", len(nodes.Items))
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != theCluster {
			t.Errorf("node %s names cluster %q", node.Name, node.Spec.ClusterRef)
		}
		if node.Spec.NodeSet != "rack-a" {
			t.Errorf("node %s does not trace back to its node set: %q", node.Name, node.Spec.NodeSet)
		}
		// The sizing is the cluster's, copied in, so the node records the layout
		// it was built with and nothing refers back to the document.
		if node.Spec.Config.Sizing.VCPUCount == nil || *node.Spec.Config.Sizing.VCPUCount != 8 {
			t.Errorf("node %s did not take the cluster's sizing", node.Name)
		}
		if len(node.Spec.Config.DeviceNames) != 1 ||
			node.Spec.Config.DeviceNames[0] != "0000:5e:00.0" {
			t.Errorf("node %s did not take the group's devices: %v",
				node.Name, node.Spec.Config.DeviceNames)
		}
	}
}

// The step the expansion cannot get wrong: a crash part-way through creates the
// rest on the next pass and duplicates nothing.
func TestCreatingNodesIsIdempotent(t *testing.T) {
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	for pass := 0; pass < 3; pass++ {
		if _, err := r.createNodes(context.Background(), config); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing the nodes: %v", err)
	}
	if len(nodes.Items) != 2 {
		t.Fatalf("three passes produced %d nodes, want the same two", len(nodes.Items))
	}
}

// A two-socket layout produces one node per socket per worker, and each carries
// the socket it is bound to.
func TestATwoSocketLayoutProducesTwoNodesPerWorker(t *testing.T) {
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{
			SocketsToUse:   []string{"0", "1"},
			NodesPerSocket: ptr.To(int32(1)),
		}
	})
	objects := append(workers("worker-1", "worker-2"), config, cluster)
	r := reconcilerFor(t, objects...)

	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing the nodes: %v", err)
	}
	if len(nodes.Items) != 4 {
		t.Fatalf("created %d nodes, want two workers by two sockets", len(nodes.Items))
	}

	sockets := map[string]int{}
	for i := range nodes.Items {
		sockets[nodes.Items[i].Spec.SocketID]++
	}
	if sockets["0"] != 2 || sockets["1"] != 2 {
		t.Errorf("the nodes are not spread over both sockets: %v", sockets)
	}
}

// A growth document adds only the slots that are not filled, which is what makes
// adding a rack a second document rather than an edit of the first.
func TestAGrowthDocumentAddsOnlyTheMissingNodes(t *testing.T) {
	existing := &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: "already-there", Namespace: theNamespace},
		Spec: simplyblockv1alpha2.StorageNodeSpec{
			ClusterRef: theCluster,
			WorkerNode: "worker-1",
			Slot:       ptr.To(int32(0)),
		},
	}
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil), existing)
	r := reconcilerFor(t, objects...)

	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing the nodes: %v", err)
	}
	if len(nodes.Items) != 2 {
		t.Fatalf("created %d nodes, want the one that was missing", len(nodes.Items))
	}
	for i := range nodes.Items {
		if nodes.Items[i].Name != "already-there" &&
			nodes.Items[i].Spec.WorkerNode != "worker-2" {
			t.Errorf("the new node is on %s, want the unfilled worker",
				nodes.Items[i].Spec.WorkerNode)
		}
	}
}

// A draft naming a worker that is not there is reported rather than expanded,
// which is the whole value of the review gate.
func TestADraftNamingAMissingWorkerIsReported(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
	})
	objects := append(workers("worker-1"), config)
	r := reconcilerFor(t, objects...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(findings) != 1 || findings[0].reason != WorkerNotFound {
		t.Fatalf("findings = %+v, want one WorkerNotFound", findings)
	}
	if !strings.Contains(findings[0].message, "worker-2") {
		t.Errorf("the finding does not name the missing worker: %s", findings[0].message)
	}
}

// A valid draft produces nothing to report, which is what AwaitingApproval is
// emitted against.
func TestAValidDraftHasNoFindings(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
	})
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("a valid draft reported %+v", findings)
	}
}

// A growth document whose groups name the other class is reported while it is
// still editable, because approving it is what makes it immutable.
func TestAGrowthDocumentOfTheWrongClassIsReported(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.ClusterRef = theCluster
		c.Spec.Cluster = nil
		c.Spec.NodeSets[0].Groups[0].Devices = &simplyblockv1alpha2.DeviceSelection{
			Block: []string{"/dev/sdb"},
		}
	})
	cluster := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.DeviceClass = simplyblockv1alpha2.StorageClusterDeviceClassNVMe
	})
	objects := append(workers("worker-1", "worker-2"), config, cluster)
	r := reconcilerFor(t, objects...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(findings) != 1 || findings[0].reason != DeviceClassMismatch {
		t.Fatalf("findings = %+v, want one DeviceClassMismatch", findings)
	}
}

// A cluster with failure domains enabled needs every group to name one, and
// saying so at draft time turns an investigation into an edit.
func TestFailureDomainsAreCheckedAgainstTheTemplate(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.EnableFailureDomains = ptr.To(true)
	})
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the missing fault group", findings)
	}
	if !strings.Contains(findings[0].message, "rack-a/saturn") {
		t.Errorf("the finding does not name the group: %s", findings[0].message)
	}
}

// The environment is a shorthand and the expansion is where it is spent: naming
// OpenShift once decides the distribution flags, after which nothing reads it.
func TestTheEnvironmentResolvesIntoTheWorkloadFlags(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentOpenShift
	})
	r := reconcilerFor(t)

	workload := r.buildWorkload(config)
	if workload.OpenShiftCluster == nil || !*workload.OpenShiftCluster {
		t.Error("OpenShift did not set openShiftCluster")
	}
	if workload.EnableCpuTopology == nil || !*workload.EnableCpuTopology {
		t.Error("OpenShift did not set enableCpuTopology")
	}
}

// Every node of one document gets a name of its own, because the name is derived
// from the cluster, the worker, and the slot.
func TestEveryNodeGetsADistinctName(t *testing.T) {
	seen := map[string]struct{}{}
	for _, worker := range []string{"worker-1", "worker-2"} {
		for slot := int32(0); slot < 2; slot++ {
			name := nodeName(theCluster, worker, slot)
			if _, clash := seen[name]; clash {
				t.Fatalf("two slots derived the same name %q", name)
			}
			seen[name] = struct{}{}
		}
	}
}
