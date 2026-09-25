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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
//
// It states 1+0 because two workers carry no other scheme: every redundant one
// needs at least three storage nodes, and a document that says nothing about
// erasure coding means the control plane's 1+1. A case about the stripe says so
// by overwriting the field.
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
				Stripe: &simplyblockv1alpha2.StripeSpec{
					DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(0)),
				},
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

// aCluster is a StorageCluster the control plane has already created. It states
// the 1+0 the document fixture states, and for the same reason: a growth
// document is answered against its cluster's stripe, and two workers carry no
// redundant scheme.
func aCluster(
	mutate func(*simplyblockv1alpha2.StorageCluster),
) *simplyblockv1alpha2.StorageCluster {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: theCluster, Namespace: theNamespace},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			VCPUCount:        ptr.To(int32(8)),
			MinHugePagesSize: "100G",
			Stripe: &simplyblockv1alpha2.StripeSpec{
				DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(0)),
			},
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

// The host OS is spent the same way the environment is, and on the one flag it
// decides: Ubuntu keeps the NVMe-oF modules in a package the base install does
// not carry, so a storage node on one has to install it and a node on anything
// else must not be told to try.
func TestTheHostOSResolvesIntoUbuntuHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hostOS *simplyblockv1alpha2.HostOSSpec
		want   bool
	}{
		{"ubuntu", &simplyblockv1alpha2.HostOSSpec{
			Distro: simplyblockv1alpha2.DistroUbuntu,
			Family: simplyblockv1alpha2.HostOSFamilyDebian,
		}, true},
		// Debian is the family and not the distribution, and the package is
		// Ubuntu's: a Debian host has the modules already.
		{"debian", &simplyblockv1alpha2.HostOSSpec{
			Distro: "debian",
			Family: simplyblockv1alpha2.HostOSFamilyDebian,
		}, false},
		{"rocky", &simplyblockv1alpha2.HostOSSpec{
			Distro: "rocky",
			Family: simplyblockv1alpha2.HostOSFamilyRedHat,
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
				c.Spec.HostOS = tc.hostOS
			})

			workload := reconcilerFor(t).buildWorkload(config)
			if workload.UbuntuHost == nil {
				t.Fatalf("a document stating %s left ubuntuHost unset", tc.hostOS.Distro)
			}
			if *workload.UbuntuHost != tc.want {
				t.Errorf("a document stating %s set ubuntuHost to %v, want %v",
					tc.hostOS.Distro, *workload.UbuntuHost, tc.want)
			}
		})
	}
}

// A document that states no host OS states nothing about ubuntuHost either. It
// is not the same as stating a host that is not Ubuntu: the cluster falls back
// to its own default, which is what a hand-written cluster gets, and a reviewer
// who knows better can still set it.
func TestADocumentWithNoHostOSLeavesUbuntuHostUnset(t *testing.T) {
	workload := reconcilerFor(t).buildWorkload(aDocument(func(*simplyblockv1alpha2.ClusterDeploymentConfig) {}))
	if workload.UbuntuHost != nil {
		t.Errorf("ubuntuHost is %v with no host OS stated, want unset", *workload.UbuntuHost)
	}
}

// Where the storage-node pods are allowed to run is the document's to state.
// A fleet that taints its storage plane, which is how a machine is dedicated
// to one workload, has a DaemonSet that schedules nowhere without this, and a
// document that could not say so left an administrator editing the cluster the
// document had just written.
func TestTheDocumentsTolerationsReachTheStorageNodes(t *testing.T) {
	tolerations := []corev1.Toleration{{
		Key:      "io.simplyblock.node-type",
		Operator: corev1.TolerationOpEqual,
		Value:    "storage-plane",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.Tolerations = tolerations
	})

	workload := reconcilerFor(t).buildWorkload(config)
	if !reflect.DeepEqual(workload.Tolerations, tolerations) {
		t.Errorf("the cluster tolerates %+v, want %+v", workload.Tolerations, tolerations)
	}
}

// A growth document names a cluster instead of describing one, and that cluster
// already states what its storage nodes tolerate. There is no template to read
// them from, and re-stating them would be a second answer to a settled
// question.
func TestAGrowthDocumentStatesNoTolerations(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster = nil
		c.Spec.ClusterRef = theCluster
	})

	if workload := reconcilerFor(t).buildWorkload(config); len(workload.Tolerations) != 0 {
		t.Errorf("a growth document produced %+v", workload.Tolerations)
	}
}

// What the storage-node container is sized with. The default is the agent's
// modest one, and a fleet whose nodes serve many subsystems outgrows it: a
// document that could not say so left the sizing to an edit of the cluster it
// had just written.
func TestTheDocumentsContainerResourcesReachTheStorageNodes(t *testing.T) {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.ContainerResources = &resources
	})

	workload := reconcilerFor(t).buildWorkload(config)
	if !workload.ContainerResources.Requests.Cpu().Equal(resource.MustParse("500m")) {
		t.Errorf("the container requests %v, want 500m", workload.ContainerResources.Requests.Cpu())
	}
	if !workload.ContainerResources.Limits.Memory().Equal(resource.MustParse("4Gi")) {
		t.Errorf("the container is limited to %v, want 4Gi", workload.ContainerResources.Limits.Memory())
	}
}

// A document that states no resources states nothing, and the cluster's own
// defaults decide. Stating an empty block would be a third answer beside the
// default and the stated one.
func TestADocumentWithNoContainerResourcesLeavesTheClustersUnset(t *testing.T) {
	config := aDocument(func(*simplyblockv1alpha2.ClusterDeploymentConfig) {})

	workload := reconcilerFor(t).buildWorkload(config)
	if len(workload.ContainerResources.Requests) != 0 || len(workload.ContainerResources.Limits) != 0 {
		t.Errorf("the cluster is sized %+v with nothing stated", workload.ContainerResources)
	}
}

// The init containers are sized separately, because they do a different job:
// one writes an env file and the other runs node_configure.py once, and both
// are done before the container the fleet's sizing is about starts.
func TestTheDocumentsInitContainerResourcesReachTheStorageNodes(t *testing.T) {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.InitContainerResources = &resources
	})

	workload := reconcilerFor(t).buildWorkload(config)
	if !workload.InitContainerResources.Limits.Memory().Equal(resource.MustParse("1Gi")) {
		t.Errorf("the init containers are limited to %v, want 1Gi",
			workload.InitContainerResources.Limits.Memory())
	}
	// The two blocks are independent: sizing the init containers says nothing
	// about the container that runs for the node's life.
	if len(workload.ContainerResources.Requests) != 0 {
		t.Errorf("sizing the init containers also sized the container: %+v", workload.ContainerResources)
	}
}

func TestADocumentWithNoInitContainerResourcesLeavesTheClustersUnset(t *testing.T) {
	config := aDocument(func(*simplyblockv1alpha2.ClusterDeploymentConfig) {})

	workload := reconcilerFor(t).buildWorkload(config)
	if len(workload.InitContainerResources.Requests) != 0 || len(workload.InitContainerResources.Limits) != 0 {
		t.Errorf("the init containers are sized %+v with nothing stated", workload.InitContainerResources)
	}
}

// The CPUs held back from SPDK are stated per group, because they are a list of
// core ids: a group of sixteen-core workers and a group of ninety-six-core
// workers have no one list that is right for both, and a group is what a
// document calls machines that share their hardware.
func TestTheGroupsReservedCPUsReachEveryNodeOfIt(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets[0].Groups[0].ReservedSystemCPU = "0-3"
	})
	cluster := aCluster(nil)
	r := reconcilerFor(t)

	node := r.buildNode(config, cluster,
		config.Spec.NodeSets[0], config.Spec.NodeSets[0].Groups[0], "worker-1", 0)

	if got := node.Spec.Config.ReservedSystemCPU; got != "0-3" {
		t.Errorf("the node holds back %q, want the group's %q", got, "0-3")
	}
}

// A group that states none leaves the node stating none, so that the cluster's
// fleet-wide value is what the agent reads. Writing an empty string would be
// the same as stating one.
func TestAGroupWithNoReservedCPUsStatesNone(t *testing.T) {
	config := aDocument(func(*simplyblockv1alpha2.ClusterDeploymentConfig) {})
	cluster := aCluster(nil)
	r := reconcilerFor(t)

	node := r.buildNode(config, cluster,
		config.Spec.NodeSets[0], config.Spec.NodeSets[0].Groups[0], "worker-1", 0)

	if got := node.Spec.Config.ReservedSystemCPU; got != "" {
		t.Errorf("the node holds back %q with nothing stated", got)
	}
}
