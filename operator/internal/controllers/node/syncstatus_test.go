// What a provisioned node's object says about it, and where that comes from.
//
// Steady state is the stream's cache: the control plane pushes a node's changes,
// so reading one back costs no request, and the poll that remains is the
// correctness floor for a stream nobody has noticed is dead. The fallback matters
// as much as the cache — a node the stream has not delivered still has to be
// readable, or a cold cache would leave the object's status frozen until the
// first snapshot lands.
//
// Occupancy is the one figure in neither the node list nor the stream. It exists
// only in the metrics the control plane exports, and it is written under
// hysteresis for a reason that is not economy: the reconciler watches its own
// objects, so writing a freshly sampled number every pass would make a node
// reconcile itself in a loop for as long as any I/O was happening.
//
// design-storagenode.md §3.3, §4.4, and §12.

package node

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// sampledCapacity is a metrics endpoint that answers with what a test scripted,
// or fails the way one that is momentarily away does.
type sampledCapacity struct {
	samples map[string]prometheus.Capacity
	err     error
}

func (s *sampledCapacity) NodeCapacity(
	_ context.Context, _ string,
) (map[string]prometheus.Capacity, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.samples, nil
}

// aSteadyNode builds the entity reconciler over a provisioned node and its
// cluster.
func aSteadyNode(
	t *testing.T, api ControlPlane, objects ...client.Object,
) (*StorageNodeReconciler, client.Client) {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme)

	node := anOpsNode()
	node.Finalizers = []string{NodeFinalizer}
	world := append([]client.Object{node, anOpsCluster()}, objects...)

	apiClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(world...).
		WithStatusSubresource(
			&simplyblockv1alpha2.StorageNode{},
			&simplyblockv1alpha2.StorageNodeOps{},
			&simplyblockv1alpha2.StorageCluster{},
		).
		WithIndex(&simplyblockv1alpha2.StorageNode{}, clusterRefField,
			func(o client.Object) []string {
				return []string{o.(*simplyblockv1alpha2.StorageNode).Spec.ClusterRef}
			}).
		Build()

	return &StorageNodeReconciler{
		Client:   apiClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(64),
		API:      api,
		Workload: &Workload{Client: apiClient},
	}, apiClient
}

// nodeRead is the node as the API server now holds it.
func nodeRead(t *testing.T, apiClient client.Client) *simplyblockv1alpha2.StorageNode {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	return &node
}

// settle runs one reconcile of the node.
func settle(t *testing.T, r *StorageNodeReconciler) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName},
	})
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}
	return result
}

// A node the stream has delivered is read from the cache, and the control plane
// is not asked for what it has already pushed.
func TestAPushedNodeIsReadFromTheStreamRatherThanAskedFor(t *testing.T) {
	api := aControlPlane()
	r, apiClient := aSteadyNode(t, api)
	r.Nodes = &deliveredNodes{synced: true, nodes: []subscriptions.NodeDTO{{
		ID: opsNodeID, Status: nodeStatusOnline, ManagementIP: "192.168.10.112",
		HealthCheck: true, Hostname: "vm02_4420", CPUCount: 6, Volumes: 3,
		RPCPort: 4420, LvolPort: 4426, NVMeOFPort: 4421,
	}}}

	settle(t, r)

	if asked := api.asked("StorageNode"); asked != 0 {
		t.Errorf("the control plane was asked %d time(s) for a node the stream delivered", asked)
	}
	node := nodeRead(t, apiClient)
	if node.Status.Status != nodeStatusOnline || !node.Status.Health {
		t.Errorf("status/health = %q/%v, want what the stream reported",
			node.Status.Status, node.Status.Health)
	}
	if node.Status.Hostname != "vm02_4420" {
		t.Errorf("hostname = %q; the control plane's name for a node is not the worker's",
			node.Status.Hostname)
	}
	if node.Status.Ports == nil || node.Status.Ports.Management != "192.168.10.112" {
		t.Errorf("ports = %+v, want the addresses the stream carried", node.Status.Ports)
	}
	if node.Status.Resources == nil || ptr.From(node.Status.Resources.Volumes, 0) != 3 {
		t.Errorf("resources = %+v, want the volume count the stream carried", node.Status.Resources)
	}
	if node.Status.Phase != simplyblockv1alpha2.StorageNodePhaseOnline {
		t.Errorf("phase = %q, want Online", node.Status.Phase)
	}
}

// A node the stream has not delivered is read from the control plane, because a
// cold cache must not freeze a node's status until the first snapshot lands.
func TestANodeTheStreamHasNotDeliveredIsAskedFor(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusInCreation)
	r, apiClient := aSteadyNode(t, api)
	r.Nodes = &deliveredNodes{synced: false}

	settle(t, r)

	if asked := api.asked("StorageNode"); asked != 1 {
		t.Errorf("the control plane was asked %d time(s), want once for an undelivered node",
			asked)
	}
	if phase := nodeRead(t, apiClient).Status.Phase; phase !=
		simplyblockv1alpha2.StorageNodePhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning for a node the control plane is still creating",
			phase)
	}
}

// The phase is the operator's reading of the lifecycle the control plane
// reports, and the two vocabularies are deliberately separate.
func TestThePhaseIsThisOperatorsReadingOfWhatTheControlPlaneSays(t *testing.T) {
	cases := []struct {
		reading NodeReading
		want    simplyblockv1alpha2.StorageNodePhase
	}{
		{NodeReading{Status: nodeStatusOnline}, simplyblockv1alpha2.StorageNodePhaseOnline},
		{NodeReading{Status: nodeStatusActive}, simplyblockv1alpha2.StorageNodePhaseOnline},
		{
			NodeReading{Status: nodeStatusOnline, DevicesCount: 4, OnlineDevicesCount: 3},
			simplyblockv1alpha2.StorageNodePhaseDegraded,
		},
		{NodeReading{Status: nodeStatusSuspended}, simplyblockv1alpha2.StorageNodePhaseOffline},
		{NodeReading{Status: nodeStatusOffline}, simplyblockv1alpha2.StorageNodePhaseOffline},
		{
			NodeReading{Status: nodeStatusInCreation},
			simplyblockv1alpha2.StorageNodePhaseProvisioning,
		},
		{
			NodeReading{Status: nodeStatusInRestart},
			simplyblockv1alpha2.StorageNodePhaseProvisioning,
		},
		{NodeReading{Status: "unreachable"}, simplyblockv1alpha2.StorageNodePhaseFailed},
		{NodeReading{Status: "a status this operator has never heard of"},
			simplyblockv1alpha2.StorageNodePhaseFailed},
	}
	for _, c := range cases {
		if got := phaseOf(c.reading); got != c.want {
			t.Errorf("a node reporting %q with %d of %d devices online is %q, want %q",
				c.reading.Status, c.reading.OnlineDevicesCount, c.reading.DevicesCount,
				got, c.want)
		}
	}
}

// A stored UUID the control plane no longer reports is a cluster that was reset
// and its nodes recreated. The object goes back to provisioning rather than
// reporting a node that is not there.
func TestANodeTheControlPlaneHasForgottenGoesBackToProvisioning(t *testing.T) {
	api := aControlPlane()
	delete(api.nodes, opsNodeID)
	r, apiClient := aSteadyNode(t, api)

	settle(t, r)

	node := nodeRead(t, apiClient)
	if node.Status.UUID != "" {
		t.Errorf("the object still names backend node %q, which the control plane has forgotten",
			node.Status.UUID)
	}
	if node.Status.Phase != simplyblockv1alpha2.StorageNodePhasePending {
		t.Errorf("phase = %q, want Pending", node.Status.Phase)
	}
}

// The first sample is always worth writing: an object that says nothing about
// its occupancy says nothing a reader can use.
func TestAFirstCapacityReadingIsAlwaysWritten(t *testing.T) {
	sampled := prometheus.Capacity{
		Total: 112303538176, Used: 422576128, SampledAt: time.Unix(1788423117, 0).UTC(),
	}
	r, apiClient := aSteadyNode(t, aControlPlane())
	r.Capacity = &sampledCapacity{samples: map[string]prometheus.Capacity{opsNodeID: sampled}}

	settle(t, r)

	capacity := nodeRead(t, apiClient).Status.Resources.Capacity
	if capacity == nil || capacity.UsedBytes == nil || capacity.TotalBytes == nil {
		t.Fatalf("capacity = %+v, want the sample that was taken", capacity)
	}
	if *capacity.UsedBytes != sampled.Used || *capacity.TotalBytes != sampled.Total {
		t.Errorf("capacity = %d of %d, want %d of %d",
			*capacity.UsedBytes, *capacity.TotalBytes, sampled.Used, sampled.Total)
	}
	if capacity.SampledAt == nil {
		t.Error("nothing records when the sample was taken, so its age cannot be judged")
	}
}

// Whether a sample is worth writing is decided against what the object already
// says, because every write schedules another reconcile.
func TestASampleIsWrittenOnlyWhenItSaysSomethingNew(t *testing.T) {
	const total = int64(100_000_000_000)
	recorded := &simplyblockv1alpha2.StorageNodeCapacity{
		TotalBytes: ptr.To(total), UsedBytes: ptr.To(int64(50_000_000_000)),
	}

	cases := []struct {
		name   string
		sample prometheus.Capacity
		want   bool
	}{
		{
			"a use that barely moved",
			prometheus.Capacity{Total: total, Used: 50_100_000_000, SampledAt: time.Now()},
			false,
		},
		{
			"a use that moved by a percent of the node",
			prometheus.Capacity{Total: total, Used: 51_000_000_000, SampledAt: time.Now()},
			true,
		},
		{
			"a total that changed, which is a device joining or leaving",
			prometheus.Capacity{Total: total + 1, Used: 50_000_000_000, SampledAt: time.Now()},
			true,
		},
		{
			"nothing measured this node at all",
			prometheus.Capacity{},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := worthWriting(recorded, c.sample); got != c.want {
				t.Errorf("worthWriting = %v, want %v", got, c.want)
			}
		})
	}

	if !worthWriting(nil, prometheus.Capacity{Total: total, Used: 1, SampledAt: time.Now()}) {
		t.Error("a first reading was not written, so the object would say nothing about occupancy")
	}
}

// A capacity source that is momentarily away is not a reason to publish nothing:
// the rest of the status is correct without it.
func TestAFailingCapacitySourceStillLeavesTheNodePublished(t *testing.T) {
	r, apiClient := aSteadyNode(t, aControlPlane())
	r.Capacity = &sampledCapacity{err: errors.New("the metrics endpoint is away")}

	settle(t, r)

	node := nodeRead(t, apiClient)
	if node.Status.Phase != simplyblockv1alpha2.StorageNodePhaseOnline {
		t.Errorf("phase = %q, want the node published without its occupancy", node.Status.Phase)
	}
	if node.Status.Resources != nil && node.Status.Resources.Capacity != nil {
		t.Errorf("capacity = %+v, want nothing said about a figure nobody measured",
			node.Status.Resources.Capacity)
	}
}

// A node nothing sampled carries no capacity, rather than a zero one that reads
// as an empty node.
func TestANodeNobodySampledCarriesNoCapacity(t *testing.T) {
	r, apiClient := aSteadyNode(t, aControlPlane())
	r.Capacity = &sampledCapacity{samples: map[string]prometheus.Capacity{
		"another-node": {Total: 1, Used: 1, SampledAt: time.Now()},
	}}

	settle(t, r)

	node := nodeRead(t, apiClient)
	if node.Status.Resources != nil && node.Status.Resources.Capacity != nil {
		t.Errorf("capacity = %+v, want none for a node nothing sampled",
			node.Status.Resources.Capacity)
	}
}

// A cordoned worker takes its storage-node pod with it, so the node is taken
// down deliberately rather than killed underneath a running SPDK process. The
// operator raises the window; a user does not have to.
func TestACordonedWorkerRaisesItsMaintenanceWindow(t *testing.T) {
	cordoned := aWorker(opsWorker, true)
	cordoned.Spec.Unschedulable = true
	r, apiClient := aSteadyNode(t, aControlPlane(), cordoned)

	settle(t, r)

	var window simplyblockv1alpha2.StorageNodeOps
	key := client.ObjectKey{Namespace: opsNamespace, Name: "a-node-maintenance"}
	if err := apiClient.Get(context.Background(), key, &window); err != nil {
		t.Fatalf("reading the maintenance window: %v", err)
	}
	if window.Spec.Action != simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance {
		t.Errorf("the operation raised is a %s", window.Spec.Action)
	}
	if len(window.OwnerReferences) != 1 || window.OwnerReferences[0].Name != opsNodeName {
		t.Errorf("owner references = %v, want the node that raised it for itself",
			window.OwnerReferences)
	}

	// The second pass finds the window it raised rather than raising another.
	settle(t, r)
	var windows simplyblockv1alpha2.StorageNodeOpsList
	if err := apiClient.List(context.Background(), &windows); err != nil {
		t.Fatalf("listing the operations: %v", err)
	}
	if len(windows.Items) != 1 {
		t.Errorf("%d windows were raised for one cordon", len(windows.Items))
	}
}

// A node with no backend behind it has nothing to drain, so the object goes
// straight away.
func TestANodeThatWasNeverProvisionedIsDeletedOutright(t *testing.T) {
	r, apiClient := aSteadyNode(t, aControlPlane())
	node := nodeRead(t, apiClient)
	node.Status.UUID = ""
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("clearing the node's UUID: %v", err)
	}
	if err := apiClient.Delete(context.Background(), node); err != nil {
		t.Fatalf("deleting the node: %v", err)
	}

	settle(t, r)

	var gone simplyblockv1alpha2.StorageNode
	err := apiClient.Get(context.Background(),
		client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}, &gone)
	if err == nil {
		t.Errorf("the object is still there with finalizers %v", gone.Finalizers)
	}
}

// A node with data on it is drained before the object goes, by a Remove
// operation the node owns, and the finalizer is held until that operation has
// finished.
//
// Regression: 2026-09-17-node-teardown-reads-an-unheld-lock-as-a-finished-drain.
// The teardown held the finalizer while status.activeOpsRef was set, and a drain
// raised one line earlier has not taken the lock yet — so the first teardown pass
// read "not started" as "finished" and dropped the finalizer immediately. The
// object went, the operation it owns went with it, and the backend node was left
// running with its data on it and nothing in Kubernetes tracking it.
func TestANodeWithDataOnItIsDrainedBeforeItGoes(t *testing.T) {
	r, apiClient := aSteadyNode(t, aControlPlane())
	node := nodeRead(t, apiClient)
	if err := apiClient.Delete(context.Background(), node); err != nil {
		t.Fatalf("deleting the node: %v", err)
	}

	settle(t, r)

	drain := drainRaisedFor(t, apiClient)
	if drain.Spec.Action != simplyblockv1alpha2.StorageNodeOpsActionRemove {
		t.Errorf("the operation raised is a %s", drain.Spec.Action)
	}
	if len(drain.OwnerReferences) != 1 || drain.OwnerReferences[0].Name != opsNodeName {
		t.Errorf("owner references = %v, want the node that raised it for itself",
			drain.OwnerReferences)
	}
	if len(finalizersOn(t, apiClient)) == 0 {
		t.Fatal("the finalizer came off on the pass that raised the drain, so the object " +
			"goes while the backend node is still there with its data on it")
	}

	// A drain that is running holds it too, which is the long middle of a real
	// removal.
	finishDrain(t, apiClient, simplyblockv1alpha2.StorageNodeOpsPhaseRunning)
	settle(t, r)
	if len(finalizersOn(t, apiClient)) == 0 {
		t.Fatal("the finalizer came off while the drain was still running")
	}

	// A drain that is over releases it, whatever its outcome: the operation stays
	// as the record, and an object nobody can delete would be worse.
	finishDrain(t, apiClient, simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded)
	settle(t, r)

	var gone simplyblockv1alpha2.StorageNode
	err := apiClient.Get(context.Background(),
		client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}, &gone)
	if err == nil {
		t.Errorf("the object is still held by %v after its drain finished", gone.Finalizers)
	}
}

// finalizersOn is what still holds the node, and nothing at all when the object
// has already gone.
func finalizersOn(t *testing.T, apiClient client.Client) []string {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	err := apiClient.Get(context.Background(), key, &node)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	return node.Finalizers
}

// drainRaisedFor reads the removal the node raised for itself.
func drainRaisedFor(
	t *testing.T, apiClient client.Client,
) *simplyblockv1alpha2.StorageNodeOps {
	t.Helper()
	var drain simplyblockv1alpha2.StorageNodeOps
	key := client.ObjectKey{Namespace: opsNamespace, Name: "a-node-remove"}
	if err := apiClient.Get(context.Background(), key, &drain); err != nil {
		t.Fatalf("reading the drain: %v", err)
	}
	return &drain
}

// finishDrain moves the removal to the phase a case is about.
func finishDrain(
	t *testing.T, apiClient client.Client, phase simplyblockv1alpha2.StorageNodeOpsPhase,
) {
	t.Helper()
	drain := drainRaisedFor(t, apiClient)
	drain.Status.Phase = phase
	if err := apiClient.Status().Update(context.Background(), drain); err != nil {
		t.Fatalf("moving the drain to %s: %v", phase, err)
	}
}
