// Validation of a document that describes a supercluster: two or more node
// sets, each becoming a member cluster (design-superclusters.md §5.1 to §5.3).
// What spans one cluster spans one node set, and each node set is held to the
// limits of a cluster.

package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aNodeSet is one node set of one group on the given workers, on the given
// interfaces, with three block devices per worker.
func aNodeSet(name, mgmt string, workerNames ...string) simplyblockv1alpha2.NodeSet {
	return simplyblockv1alpha2.NodeSet{
		Name: name,
		Groups: []simplyblockv1alpha2.NodeGroup{{
			Name:           "group-1",
			Workers:        workerNames,
			MgmtInterface:  mgmt,
			DataInterfaces: []string{mgmt + "-data"},
			Devices: &simplyblockv1alpha2.DeviceSelection{
				Block: []string{"/dev/sdb", "/dev/sdc", "/dev/sdd"},
			},
		}},
	}
}

// aSuperclusterDocument is a draft of two node sets of one worker each, on two
// different networks, as single-node members striped 1+1 across two of their
// three block devices, the third being the journal device block devices need.
func aSuperclusterDocument(
	mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig),
) *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = false
		c.Spec.Cluster.ClusterMode = simplyblockv1alpha2.ClusterModeSingle
		c.Spec.Cluster.EnableJournalDevice = ptr.To(true)
		c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(1)),
		}
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			aNodeSet("fast", "eth1", "worker-1"),
			aNodeSet("dense", "eth3", "worker-2"),
		}
		if mutate != nil {
			mutate(c)
		}
	})
}

func TestADocumentOfTwoSingleNodeMembersOnTwoNetworksIsValid(t *testing.T) {
	if found := findingsOf(t, aSuperclusterDocument(nil), workers("worker-1", "worker-2")...); len(found) > 0 {
		t.Errorf("findings = %+v, want none", found)
	}
}

// The erasure-coding minimum is a member's, since a member stripes across its
// own nodes alone: four workers of 1+1 are enough for one cluster and not for a
// member of three and a member of one.
func TestTheStripeMinimumIsCheckedPerMember(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.ClusterMode = ""
		c.Spec.Cluster.Stripe = &simplyblockv1alpha2.StripeSpec{
			DataChunks: ptr.To(int32(1)), ParityChunks: ptr.To(int32(1)),
		}
		c.Spec.NodeSets = []simplyblockv1alpha2.NodeSet{
			aNodeSet("fast", "eth1", "worker-1", "worker-2", "worker-3"),
			aNodeSet("dense", "eth1", "worker-4"),
		}
	})

	found := only(t, findingsOf(t, config, workers("worker-1", "worker-2", "worker-3", "worker-4")...),
		StripeBelowMinimumNodes)
	if !strings.Contains(found.message, "dense") {
		t.Errorf("the finding does not name the node set that is short: %s", found.message)
	}
}

// A cluster holds at most 32 storage nodes, a member as much as any other.
func TestANodeSetOverTheStorageNodeLimitIsReported(t *testing.T) {
	names := make([]string, 0, 33)
	for i := 1; i <= 33; i++ {
		names = append(names, fmt.Sprintf("worker-%d", i))
	}
	for _, sets := range [][]simplyblockv1alpha2.NodeSet{
		{aNodeSet("large", "eth1", names...)},
		{aNodeSet("large", "eth1", names...), aNodeSet("small", "eth1", "worker-99")},
	} {
		config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Approved = false
			c.Spec.NodeSets = sets
		})

		var limit *finding
		for _, f := range findingsOf(t, config, workers(append(names, "worker-99")...)...) {
			if f.reason == MemberTooLarge {
				limit = &f
			}
		}
		if limit == nil || !strings.Contains(limit.message, "large") || !strings.Contains(limit.message, "33") {
			t.Errorf("%d node sets: MemberTooLarge = %+v, want it to name node set large and its 33 nodes", len(sets), limit)
		}
	}
}

// A member is named <cluster>-<nodeset>-<suffix>, which a StorageCluster name
// must hold to, so a document whose names do not fit says so before approval.
func TestAMemberNameThatCannotBeANameIsReported(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.Name = strings.Repeat("c", 50)
		c.Spec.NodeSets[1].Name = "a-long-node-set"
	})

	found := only(t, findingsOf(t, config, workers("worker-1", "worker-2")...), MemberNameInvalid)
	if !strings.Contains(found.message, "a-long-node-set") {
		t.Errorf("the finding does not name the node set: %s", found.message)
	}
}

// Every cluster records workload names of its own (§6.1) and members share
// their supercluster's (§6.2), so a namespace holding a cluster takes another.
func TestADocumentBesideAnotherClusterIsValid(t *testing.T) {
	occupant := &simplyblockv1alpha2.StorageCluster{ObjectMeta: metav1.ObjectMeta{Name: "already-there", Namespace: theNamespace}}
	objects := append(workers("worker-1", "worker-2"), occupant, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unused"}})

	if found := findingsOf(t, aSuperclusterDocument(nil), objects...); len(found) > 0 {
		t.Errorf("findings = %+v, want none", found)
	}
}

// expandingDocument is an approved supercluster document, as CreatingCluster
// reads it.
func expandingDocument() *simplyblockv1alpha2.ClusterDeploymentConfig {
	return aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Approved = true
	})
}

func readCluster(t *testing.T, r client.Reader, name string) *simplyblockv1alpha2.StorageCluster {
	t.Helper()
	var cluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: theNamespace, Name: name}, &cluster); err != nil {
		t.Fatalf("StorageCluster %s: %v", name, err)
	}
	return &cluster
}

func readDocument(t *testing.T, r client.Reader) *simplyblockv1alpha2.ClusterDeploymentConfig {
	t.Helper()
	var config simplyblockv1alpha2.ClusterDeploymentConfig
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: theNamespace, Name: "deployment"}, &config); err != nil {
		t.Fatalf("read the document: %v", err)
	}
	return &config
}

// §5.4: the supercluster is created before its members, each member's name is
// recorded before it is created, and each member is its node set's cluster.
func TestASuperclusterDocumentCreatesTheSuperclusterAndAMemberPerNodeSet(t *testing.T) {
	config := expandingDocument()
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)

	if done, err := r.createCluster(context.Background(), config); err != nil || !done {
		t.Fatalf("createCluster: done=%v err=%v", done, err)
	}

	if supercluster := readCluster(t, r, theCluster); supercluster.Spec.Supercluster == nil {
		t.Errorf("StorageCluster %s is not a supercluster", theCluster)
	}
	recorded := readDocument(t, r)
	if recorded.Status.ClusterRef != theCluster {
		t.Errorf("status.clusterRef = %q, want the supercluster", recorded.Status.ClusterRef)
	}
	if len(recorded.Status.Members) != 2 {
		t.Fatalf("status.members = %+v, want one per node set", recorded.Status.Members)
	}
	interfaces := map[string]string{"fast": "eth1", "dense": "eth3"}
	for _, entry := range recorded.Status.Members {
		if !strings.HasPrefix(entry.Cluster, theCluster+"-"+entry.NodeSet+"-") || len(entry.Cluster) != len(theCluster+"-"+entry.NodeSet+"-xyz") {
			t.Errorf("member of node set %s is named %q, want %s-%s-<suffix>", entry.NodeSet, entry.Cluster, theCluster, entry.NodeSet)
		}
		member := readCluster(t, r, entry.Cluster)
		if member.Spec.SuperclusterRef != theCluster {
			t.Errorf("member %s names supercluster %q", member.Name, member.Spec.SuperclusterRef)
		}
		if member.Spec.ClusterMode != simplyblockv1alpha2.ClusterModeSingle ||
			member.Spec.DeviceClass != simplyblockv1alpha2.StorageClusterDeviceClassLogicalBlock {
			t.Errorf("member %s: mode %q class %q, want the template's Single and its node set's block",
				member.Name, member.Spec.ClusterMode, member.Spec.DeviceClass)
		}
		if member.Spec.StorageNodes == nil || member.Spec.StorageNodes.MgmtInterface != interfaces[entry.NodeSet] {
			t.Errorf("member %s does not run on its node set's interface %s: %+v",
				member.Name, interfaces[entry.NodeSet], member.Spec.StorageNodes)
		}
	}
}

// A retry after a crash creates the members it recorded rather than drawing
// new names, so no second member of a node set is ever created.
func TestARetriedCreationCreatesTheRecordedMembers(t *testing.T) {
	config := expandingDocument()
	config.Status.Members = []simplyblockv1alpha2.ExpansionMember{
		{NodeSet: "fast", Cluster: theCluster + "-fast-kqv"},
		{NodeSet: "dense", Cluster: theCluster + "-dense-abc"},
	}
	r := reconcilerFor(t, append(workers("worker-1", "worker-2"), config)...)

	if done, err := r.createCluster(context.Background(), config); err != nil || !done {
		t.Fatalf("createCluster: done=%v err=%v", done, err)
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(context.Background(), &clusters); err != nil {
		t.Fatalf("list the clusters: %v", err)
	}
	names := map[string]bool{}
	for _, cluster := range clusters.Items {
		names[cluster.Name] = true
	}
	if len(names) != 3 || !names[theCluster+"-fast-kqv"] || !names[theCluster+"-dense-abc"] {
		t.Errorf("clusters = %v, want the supercluster and the two recorded members", names)
	}
}

// expandedMembers is the document after CreatingCluster, with its two members.
func expandedMembers(uuids bool) (*simplyblockv1alpha2.ClusterDeploymentConfig, []client.Object) {
	config := expandingDocument()
	config.Status.ClusterRef = theCluster
	config.Status.Members = []simplyblockv1alpha2.ExpansionMember{
		{NodeSet: "fast", Cluster: theCluster + "-fast-kqv"},
		{NodeSet: "dense", Cluster: theCluster + "-dense-abc"},
	}
	objects := workers("worker-1", "worker-2")
	for i, entry := range config.Status.Members {
		member := aCluster(func(c *simplyblockv1alpha2.StorageCluster) {
			c.Name = entry.Cluster
			c.Spec.SuperclusterRef = theCluster
			c.Status.UUID = ""
			if uuids || i == 0 {
				c.Status.UUID = "uuid-" + entry.NodeSet
			}
		})
		objects = append(objects, member)
	}
	return config, append(objects, config)
}

func TestAwaitingTheClusterWaitsForEveryMember(t *testing.T) {
	config, objects := expandedMembers(false)
	r := reconcilerFor(t, objects...)

	if done, err := r.awaitCluster(context.Background(), config); err != nil || done {
		t.Errorf("awaitCluster = %v, %v with one member uncreated, want to wait", done, err)
	}
}

// setNodePhase sets the phase of every node on the worker.
func setNodePhase(t *testing.T, r client.Client, worker string, phase simplyblockv1alpha2.StorageNodePhase) {
	t.Helper()
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("list the nodes: %v", err)
	}
	for i := range nodes.Items {
		if nodes.Items[i].Spec.WorkerNode == worker {
			nodes.Items[i].Status.Phase = phase
			if err := r.Update(context.Background(), &nodes.Items[i]); err != nil {
				t.Fatalf("update node: %v", err)
			}
		}
	}
}

// nodeOwners maps every worker that has a node to the cluster the node is in.
func nodeOwners(t *testing.T, r client.Reader) map[string]string {
	t.Helper()
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := r.List(context.Background(), &nodes); err != nil {
		t.Fatalf("list the nodes: %v", err)
	}
	owners := map[string]string{}
	for _, node := range nodes.Items {
		owners[node.Spec.WorkerNode] = node.Spec.ClusterRef
	}
	return owners
}

// §5.4: a member's first node adds reboot its workers on OpenShift, so the
// second member's nodes are created only once the first member's are online,
// and each member gets the nodes of its own node set.
func TestEachMemberGetsItsNodesOnceTheEarlierMembersNodesAreOnline(t *testing.T) {
	config, objects := expandedMembers(true)
	r := reconcilerFor(t, objects...)

	if done, err := r.createNodes(context.Background(), config); err != nil || done {
		t.Fatalf("createNodes = %v, %v with the first member's node not online, want to wait", done, err)
	}
	if owners := nodeOwners(t, r); len(owners) != 1 || owners["worker-1"] != theCluster+"-fast-kqv" {
		t.Fatalf("nodes by worker = %v, want worker-1 in the fast member alone", owners)
	}
	if got := readDocument(t, r).Status.NodeRefs; len(got) != 1 {
		t.Errorf("status.nodeRefs = %v, want the first member's node", got)
	}

	setNodePhase(t, r.Client, "worker-1", simplyblockv1alpha2.StorageNodePhaseOnline)
	if done, err := r.createNodes(context.Background(), readDocument(t, r)); err != nil || !done {
		t.Fatalf("createNodes = %v, %v with the first member's node online, want done", done, err)
	}
	owners := nodeOwners(t, r)
	if owners["worker-1"] != theCluster+"-fast-kqv" || owners["worker-2"] != theCluster+"-dense-abc" || len(owners) != 2 {
		t.Errorf("nodes by worker = %v, want worker-1 in the fast member and worker-2 in the dense one", owners)
	}
	if got := readDocument(t, r).Status.NodeRefs; len(got) != 2 {
		t.Errorf("status.nodeRefs = %v, want both members' nodes", got)
	}
}

// A later member whose nodes exist keeps them on record when an earlier
// member's node goes offline: the gate holds back creation, and the record
// still names every node the document built.
func TestAnEarlierMemberGoingOfflineKeepsTheLaterMembersNodesOnRecord(t *testing.T) {
	config, objects := expandedMembers(true)
	r := reconcilerFor(t, objects...)
	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}
	setNodePhase(t, r.Client, "worker-1", simplyblockv1alpha2.StorageNodePhaseOnline)
	if _, err := r.createNodes(context.Background(), readDocument(t, r)); err != nil {
		t.Fatalf("createNodes: %v", err)
	}

	setNodePhase(t, r.Client, "worker-1", simplyblockv1alpha2.StorageNodePhaseOffline)
	if _, err := r.createNodes(context.Background(), readDocument(t, r)); err != nil {
		t.Fatalf("createNodes: %v", err)
	}
	if got := readDocument(t, r).Status.NodeRefs; len(got) != 2 {
		t.Errorf("status.nodeRefs = %v, want both members' nodes", got)
	}
}

// CreatingNodes waits for every member before the last to come online, so its
// deadline grows by one activation's worth per such member, and a document of
// one cluster keeps the deadline it had.
func TestCreatingNodesIsGivenTimeForEveryEarlierMember(t *testing.T) {
	for _, tc := range []struct {
		members int
		want    time.Duration
	}{
		{1, creatingNodesDeadline},
		{4, creatingNodesDeadline + 3*activatingDeadline},
	} {
		onEnter := expansionGraph(tc.members).States[stepCreatingNodes].OnEnter
		got, err := onEnter(context.Background(), stepAwaitingCluster, stepCreatingNodes)
		if err != nil || got != tc.want {
			t.Errorf("%d member(s): deadline = %v, %v, want %v", tc.members, got, err, tc.want)
		}
	}
}

// A member is activated once its own nodes are online, without waiting for the
// other members, and the step finishes with the last.
func TestEachMemberIsActivatedOnceItsNodesAreOnline(t *testing.T) {
	config, objects := expandedMembers(true)
	r := reconcilerFor(t, objects...)
	if _, err := r.createNodes(context.Background(), config); err != nil {
		t.Fatalf("createNodes: %v", err)
	}
	setNodePhase(t, r.Client, "worker-1", simplyblockv1alpha2.StorageNodePhaseOnline)
	if _, err := r.createNodes(context.Background(), readDocument(t, r)); err != nil {
		t.Fatalf("createNodes: %v", err)
	}
	config = readDocument(t, r)

	activations := func() map[string]bool {
		var ops simplyblockv1alpha2.StorageClusterOpsList
		if err := r.List(context.Background(), &ops); err != nil {
			t.Fatalf("list the operations: %v", err)
		}
		out := map[string]bool{}
		for _, op := range ops.Items {
			out[op.Spec.ClusterRef] = true
		}
		return out
	}

	if done, err := r.activateCluster(context.Background(), config); err != nil || done {
		t.Fatalf("activateCluster = %v, %v with one member's node offline, want to wait", done, err)
	}
	if got := activations(); !got[theCluster+"-fast-kqv"] || got[theCluster+"-dense-abc"] {
		t.Errorf("activated %v, want the fast member alone", got)
	}

	setNodePhase(t, r.Client, "worker-2", simplyblockv1alpha2.StorageNodePhaseOnline)
	if done, err := r.activateCluster(context.Background(), config); err != nil || !done {
		t.Fatalf("activateCluster = %v, %v with every node online, want done", done, err)
	}
	if got := activations(); len(got) != 2 {
		t.Errorf("activated %v, want both members", got)
	}
}

// prunedMembers is a client against an apiserver whose ClusterDeploymentConfig
// schema predates status.members: it drops the field from every status write,
// as pruning does, without an error.
func prunedMembers(t *testing.T, objects ...client.Object) *ClusterDeploymentConfigReconciler {
	t.Helper()
	r := reconcilerFor(t, objects...)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, c client.Client, subResource string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			if config, ok := obj.(*simplyblockv1alpha2.ClusterDeploymentConfig); ok {
				config.Status.Members = nil
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	return r
}

// A member's name has to be on record before the member is created, or a later
// pass draws another and creates a second member of the node set. A record the
// apiserver dropped, as one with an older schema does, stops the expansion and
// creates no member.
func TestAMemberRecordThatDoesNotStickCreatesNoMember(t *testing.T) {
	config := expandingDocument()
	r := prunedMembers(t, append(workers("worker-1", "worker-2"), config)...)

	if _, err := r.createCluster(context.Background(), config); err == nil || !strings.Contains(err.Error(), "status.members") {
		t.Errorf("createCluster = %v, want an error naming status.members", err)
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(context.Background(), &clusters); err != nil {
		t.Fatalf("list the clusters: %v", err)
	}
	for _, cluster := range clusters.Items {
		if cluster.Spec.SuperclusterRef != "" {
			t.Errorf("member %s was created without its name on record", cluster.Name)
		}
	}
}

// The steps after CreatingCluster read the members off the record, and one
// that holds none is not a supercluster with nothing to wait for.
func TestTheLaterStepsRefuseASuperclusterWithoutItsMemberRecord(t *testing.T) {
	config, objects := expandedMembers(true)
	config.Status.Members = config.Status.Members[:1]
	r := reconcilerFor(t, objects...)

	if _, err := r.awaitCluster(context.Background(), config); err == nil {
		t.Error("awaitCluster passed with a node set's member missing from the record")
	}
	if _, err := r.createNodes(context.Background(), config); err == nil {
		t.Error("createNodes passed with a node set's member missing from the record")
	}
	if _, err := r.activateCluster(context.Background(), config); err == nil {
		t.Error("activateCluster passed with a node set's member missing from the record")
	}
}

// A logical block-device node carries its journal in a partition carved from
// its own smallest device by default (the journal split), and the split is per
// node, so it holds with several storage nodes per worker too. A document
// without a dedicated journal device therefore draws no finding, whatever the
// node count, and neither does one with it.
func TestBlockDevicesWithoutAJournalDeviceAreAccepted(t *testing.T) {
	config := aSuperclusterDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Cluster.EnableJournalDevice = ptr.To(false)
	})
	for _, nodes := range []int32{1, 2} {
		config.Spec.Cluster.NodesPerSocket = ptr.To(nodes)
		for _, journal := range []bool{false, true} {
			config.Spec.Cluster.EnableJournalDevice = ptr.To(journal)
			for _, f := range findingsOf(t, config, workers("worker-1", "worker-2")...) {
				if strings.Contains(strings.ToLower(f.message), "journal") {
					t.Errorf("finding %+v with %d nodes per worker and enableJournalDevice=%v, want "+
						"none about the journal", f, nodes, journal)
				}
			}
		}
	}
}
