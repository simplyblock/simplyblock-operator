// The cases a review of the expansion found, each of which was a way for a
// document to be expanded into something it did not describe.
//
// They are together in one file because they share a shape rather than a
// subsystem: every one of them was silent. A cluster bound to the wrong network,
// a fleet forced onto one socket, a growth node that looked like an initial one —
// none of them failed, and none of them said anything.

package deployment

import (
	"context"
	"errors"
	"strings"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The socket layout reaches the cluster the document creates. Without it,
// CreatingNodes read one slot off a cluster nothing had configured, and every
// deployment a document created ran one storage node per worker whatever it said.
func TestTheSocketLayoutReachesTheCreatedCluster(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.SocketsToUse = []string{"0", "1"}
		c.Spec.Cluster.NodesPerSocket = ptr.To(int32(2))
	})
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	if _, err := r.createCluster(context.Background(), config); err != nil {
		t.Fatalf("createCluster: %v", err)
	}

	var created simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: theNamespace, Name: theCluster}
	if err := r.Get(context.Background(), key, &created); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	workload := created.Spec.StorageNodes
	if workload == nil {
		t.Fatal("the cluster carries no workload")
	}
	if len(workload.SocketsToUse) != 2 {
		t.Errorf("socketsToUse = %v, want both sockets", workload.SocketsToUse)
	}
	if workload.NodesPerSocket == nil || *workload.NodesPerSocket != 2 {
		t.Errorf("nodesPerSocket = %v, want 2", workload.NodesPerSocket)
	}

	// Which is what the node count then follows from: two workers, two sockets,
	// two nodes per socket.
	config.Status.ClusterRef = theCluster
	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing the nodes: %v", err)
	}
	if len(nodes.Items) != 8 {
		t.Errorf("created %d nodes, want two workers by four slots", len(nodes.Items))
	}
}

// A create that loses a race is the ClusterExists case arriving by another route.
// Reading it as success would have the document await and then use a cluster it
// did not create and cannot prove it described.
func TestACreateThatLosesTheRaceIsRefused(t *testing.T) {
	config := aDocument(nil)
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	// The cluster appears between this run's read and its create, which is what
	// the fake client's own AlreadyExists then reports.
	if err := r.Create(context.Background(), aCluster(nil)); err != nil {
		t.Fatalf("seeding the cluster: %v", err)
	}

	_, err := r.createCluster(context.Background(), config)
	if err == nil {
		t.Fatal("a create that found the cluster already there reported success")
	}
	var refusal *refusedError
	if !errors.As(err, &refusal) || refusal.reason != ClusterExists {
		t.Errorf("the failure is %v, want a ClusterExists refusal", err)
	}
}

// OpenShift configures the kubelet. The renderer reads an unset flag as skipping
// it, so leaving the environment's resolution silent changed what an OpenShift
// deployment does.
func TestOpenShiftStatesItsKubeletFlag(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentOpenShift
	})
	r := reconcilerFor(t)

	workload := r.buildWorkload(config)
	if workload.EnableKubeletConfiguration == nil {
		t.Fatal("OpenShift left the kubelet flag unstated, which the renderer reads as skip")
	}
	if !*workload.EnableKubeletConfiguration {
		t.Error("OpenShift resolved to skipping the kubelet configuration")
	}
}

// A growth document's nodes join a cluster that is already serving, which the
// control plane reads as a request to rebalance onto them.
func TestAGrowthDocumentMarksItsNodesAsAnExpansion(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.ClusterRef = theCluster
		c.Spec.Cluster = nil
	})
	config.Status.ClusterRef = theCluster
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing the nodes: %v", err)
	}
	for i := range nodes.Items {
		expand := nodes.Items[i].Spec.Config.Expand
		if expand == nil || !*expand {
			t.Errorf("node %s is not marked as an expansion", nodes.Items[i].Name)
		}
	}
}

// A document that creates its own cluster is the initial layout, so its nodes
// leave the flag unset rather than asking for a rebalance onto themselves.
func TestAnInitialDocumentDoesNotMarkAnExpansion(t *testing.T) {
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}

	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing the nodes: %v", err)
	}
	for i := range nodes.Items {
		if nodes.Items[i].Spec.Config.Expand != nil {
			t.Errorf("node %s asks for a rebalance onto an initial layout",
				nodes.Items[i].Name)
		}
	}
}

// The record is rebuilt from the slots the document describes, so a pass that
// created a node and failed to persist the reference still reports it.
func TestTheNodeRecordSurvivesALostStatusWrite(t *testing.T) {
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}
	recorded := len(config.Status.NodeRefs)

	// The status write is lost, and the next pass finds both nodes already there.
	config.Status.NodeRefs = nil
	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("the second pass: %v", err)
	}

	if len(config.Status.NodeRefs) != recorded {
		t.Errorf("the record holds %d names after the lost write, want the %d it created",
			len(config.Status.NodeRefs), recorded)
	}
}

// A worker in two groups is a document the expansion cannot honor: the first
// group creates its nodes and the second group's devices never reach them.
func TestAWorkerInTwoGroupsIsReported(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.NodeSets[0].Groups = append(c.Spec.NodeSets[0].Groups,
			simplyblockv1alpha2.NodeGroup{
				Name:    "second",
				Workers: []string{"worker-2"},
				Devices: &simplyblockv1alpha2.DeviceSelection{NVMe: []string{"0000:88:00.0"}},
			})
	})
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !saidSomethingAbout(findings, "worker-2") {
		t.Errorf("nothing reported the repeated worker: %+v", findings)
	}
}

// One DaemonSet serves every node of a cluster, so groups that name different
// interfaces describe something the expansion cannot build.
func TestGroupsThatDisagreeAboutInterfacesAreReported(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.NodeSets[0].Groups = append(c.Spec.NodeSets[0].Groups,
			simplyblockv1alpha2.NodeGroup{
				Name:          "second",
				Workers:       []string{"worker-3"},
				MgmtInterface: "eth9",
				Devices:       &simplyblockv1alpha2.DeviceSelection{NVMe: []string{"0000:88:00.0"}},
			})
	})
	objects := append(workers("worker-1", "worker-2", "worker-3"), config)
	r := reconcilerFor(t, objects...)

	findings, err := r.validate(context.Background(), config)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !saidSomethingAbout(findings, "management interfaces") {
		t.Errorf("nothing reported the disagreement: %+v", findings)
	}
}

// An approved document waiting on the control plane is Expanding. Draft is what
// the API calls one nobody has approved, and reporting it would tell every status
// consumer the deployment is still editable.
func TestAnApprovedDocumentWaitingIsExpanding(t *testing.T) {
	config := aDocument(nil)
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	// No ControlPlane exists, so the gate holds.
	if _, err := r.Reconcile(context.Background(), requestFor(config)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var fresh simplyblockv1alpha2.ClusterDeploymentConfig
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(config), &fresh); err != nil {
		t.Fatalf("reading the document: %v", err)
	}
	if fresh.Status.Phase != simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding {
		t.Errorf("the document is %q while it waits, want Expanding", fresh.Status.Phase)
	}
}

// The marker a selector consumes is a label, because a label selector cannot see
// an annotation and the marker exists for nothing else.
func TestReadyToDeployIsALabel(t *testing.T) {
	config := aDocument(nil)
	objects := append(workers("worker-1", "worker-2"), config)
	r := reconcilerFor(t, objects...)

	if _, err := r.Reconcile(context.Background(), requestFor(config)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var fresh simplyblockv1alpha2.ClusterDeploymentConfig
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(config), &fresh); err != nil {
		t.Fatalf("reading the document: %v", err)
	}
	if fresh.Labels[readyToDeploy] != readyToDeployValue {
		t.Errorf("the approved document carries labels %v, want the marker", fresh.Labels)
	}
	if _, asAnnotation := fresh.Annotations[readyToDeploy]; asAnnotation {
		t.Error("the marker is an annotation, which no selector can see")
	}
}

// requestFor addresses one document.
func requestFor(config *simplyblockv1alpha2.ClusterDeploymentConfig) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(config)}
}

// saidSomethingAbout reports whether any finding's message names the substring.
func saidSomethingAbout(findings []finding, want string) bool {
	for _, found := range findings {
		if strings.Contains(found.message, want) {
			return true
		}
	}
	return false
}

// A document that already created its cluster does not refuse it on the next
// pass.
//
// Re-entering a step is ordinary: a lost status write, a generation bump, an
// operator restart mid-expansion. The step has to be idempotent against its own
// prior success, and this one was not — it read "a cluster by that name exists"
// and refused, on a document whose own status said it had put it there.
//
// What that cost was the deployment: the config went Failed at AwaitingCluster
// with ClusterExists, naming the cluster it had created itself, and the nodes it
// had not created yet were never created.
func TestADocumentDoesNotRefuseTheClusterItCreated(t *testing.T) {
	config := aDocument(nil)
	config.Status.ClusterRef = theCluster
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	done, err := r.createCluster(context.Background(), config)
	if err != nil {
		t.Fatalf("the document refused the cluster it created: %v", err)
	}
	if !done {
		t.Error("the step did not advance past a cluster that is already there")
	}
}

// A cluster somebody else put there is still refused, which is what the check
// exists for: the document asked to create one and nothing proves the one that
// is there is the one it described.
func TestAClusterThisDocumentDidNotCreateIsStillRefused(t *testing.T) {
	config := aDocument(nil)
	objects := append(workers("worker-1", "worker-2"), config, aCluster(nil))
	r := reconcilerFor(t, objects...)

	_, err := r.createCluster(context.Background(), config)
	if err == nil {
		t.Fatal("a cluster this document did not create was adopted silently")
	}
	var refusal *refusedError
	if !errors.As(err, &refusal) || refusal.reason != ClusterExists {
		t.Errorf("the failure is %v, want a ClusterExists refusal", err)
	}
}

// The document's drive-format decision reaches the cluster it creates.
//
// A reviewer approves a document, and what the deployment then does has to be
// what the document said. The flag is destructive and immutable on the cluster,
// so a document that states it and an expansion that drops it would format
// nothing while the draft said it would, or the reverse once somebody strikes it.
func TestTheDriveFormatDecisionReachesTheCluster(t *testing.T) {
	for _, stated := range []*bool{ptr.To(true), ptr.To(false), nil} {
		config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Cluster.EnableDriveFormat = stated
		})
		r := reconcilerFor(t)

		workload := r.buildWorkload(config)
		switch {
		case stated == nil && workload.EnableFormat4K != nil:
			t.Errorf("a document that says nothing produced %v", *workload.EnableFormat4K)
		case stated != nil && workload.EnableFormat4K == nil:
			t.Errorf("a document that said %v produced nothing", *stated)
		case stated != nil && *workload.EnableFormat4K != *stated:
			t.Errorf("the cluster got %v, want the document's %v",
				*workload.EnableFormat4K, *stated)
		}
	}
}
