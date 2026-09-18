// Relocating a node onto another worker.
//
// A migration moves the SPDK process and nothing else: the node keeps its backend
// UUID, its partitions, and its logical volumes, and what changes is the machine
// it runs on. The four steps are ordered the way they are because each of them
// guards the next — the target is put into the storage plane and its name made
// resolvable before the restart is aimed at it, the departure is observed before
// the promote, and the Kubernetes view is re-pointed only once the promote has
// landed.
//
// The promote is the point of no return, so the things that can be refused are
// refused before it: a target that is not a node of this cluster, one that is not
// Ready, and a node that is not back online.
//
// design-storagenode.md §9.

package node

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// aRelocation is the operation these cases run, aimed at opsTarget.
func aRelocation() *simplyblockv1alpha2.StorageNodeOps {
	ops := anOperation("a-relocation", simplyblockv1alpha2.StorageNodeOpsActionMigrate)
	ops.Spec.Migrate = &simplyblockv1alpha2.MigrateSpec{TargetWorkerNode: opsTarget}
	return ops
}

// aReadyStoragePod is the storage-node pod on one worker, running and ready,
// which is what Preparing waits for before it asks about DNS.
func aReadyStoragePod(worker string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "storage-node-" + worker,
			Namespace: opsNamespace,
			Labels: map[string]string{
				atlaskube.LabelApp:            atlaskube.AppStorageNode,
				atlaskube.LabelStorageNodeSet: opsCluster,
			},
		},
		Spec: corev1.PodSpec{NodeName: worker},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	return pod
}

// aPublishedName is the EndpointSlice entry the control plane resolves the
// worker's node_address through.
func aPublishedName(workers ...string) *discoveryv1.EndpointSlice {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      atlaskube.StorageNodeSetAPIEndpointSliceName(opsCluster),
			Namespace: opsNamespace,
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	for _, worker := range workers {
		hostname := utils.NodeHostnameLabel(worker)
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{
			Hostname:  &hostname,
			Addresses: []string{"10.0.0.2"},
		})
	}
	return slice
}

// aPerNodeConfig is the cluster's per-node configuration, holding one entry for
// the worker the node is leaving.
func aPerNodeConfig(entry string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PerNodeConfigMapName(opsCluster),
			Namespace: opsNamespace,
		},
		Data: map[string]string{opsWorker: entry},
	}
}

// An operation naming nowhere to go cannot be run, and no number of passes will
// give it a target.
func TestARelocationWithNoTargetEndsTheOperation(t *testing.T) {
	r, _ := anOpsWorld(t, aControlPlane())
	ops := anOperation("a-relocation", simplyblockv1alpha2.StorageNodeOpsActionMigrate)

	_, err := r.perform(context.Background(), ops, stepPreparing)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for an operation with nowhere to relocate to", err)
	}
}

// A node already on the target is what a re-run of a finished migration looks
// like. Saying so beats issuing a restart that moves a node onto the host it is
// already on.
func TestARelocationToTheHostTheNodeIsOnIsAlreadyDone(t *testing.T) {
	node := anOpsNode()
	node.Spec.WorkerNode = opsTarget
	r, apiClient := anOpsWorld(t, aControlPlane())
	replaceNode(t, apiClient, node)

	done, err := r.perform(context.Background(), aRelocation(), stepPreparing)
	if err != nil {
		t.Fatalf("preparing: %v", err)
	}
	if !done {
		t.Error("the step did not finish against a node already on the target host")
	}
}

// A target that is not a worker of this Kubernetes cluster, or one that is not
// Ready, is refused rather than waited on: both are the operation naming
// somewhere it cannot go.
func TestATargetThatCannotHostTheNodeIsRefused(t *testing.T) {
	t.Run("not a worker of this cluster", func(t *testing.T) {
		r, _ := anOpsWorld(t, aControlPlane())

		_, err := r.perform(context.Background(), aRelocation(), stepPreparing)

		var fatal *terminalStepError
		if !errors.As(err, &fatal) {
			t.Errorf("err = %v, want the terminal kind for a target that does not exist", err)
		}
	})

	t.Run("not Ready", func(t *testing.T) {
		r, _ := anOpsWorld(t, aControlPlane(), aWorker(opsTarget, false))

		_, err := r.perform(context.Background(), aRelocation(), stepPreparing)

		var fatal *terminalStepError
		if !errors.As(err, &fatal) {
			t.Errorf("err = %v, want the terminal kind for a target that is not Ready", err)
		}
	})
}

// Preparing writes the target's configuration and labels it into the storage
// plane, in that order: the entry has to exist by the time the pod's init
// container sources it.
func TestPreparingWritesTheTargetsConfigurationAndLabelsIt(t *testing.T) {
	config := aPerNodeConfig("MAX_SUBSYS_COUNT=10\nPCI_ALLOWED='0000:02:00.0'\n")
	r, apiClient := anOpsWorld(t, aControlPlane(), aWorker(opsTarget, true), config)

	ops := aRelocation()
	ops.Spec.Migrate.NewSsdPcie = []string{"0000:04:00.0"}

	done, err := r.perform(context.Background(), ops, stepPreparing)
	if err != nil {
		t.Fatalf("preparing: %v", err)
	}
	if done {
		t.Error("the step finished before the target's pod was ready")
	}

	var written corev1.ConfigMap
	key := client.ObjectKey{Namespace: opsNamespace, Name: PerNodeConfigMapName(opsCluster)}
	if err := apiClient.Get(context.Background(), key, &written); err != nil {
		t.Fatalf("reading the per-node configuration: %v", err)
	}
	entry, cloned := written.Data[opsTarget]
	if !cloned {
		t.Fatal("the target has no per-node configuration, so its init container would find none")
	}
	if entry != "MAX_SUBSYS_COUNT=10\nPCI_ALLOWED='0000:02:00.0,0000:04:00.0'\n" {
		t.Errorf("the cloned entry is %q, want the source's with the migration's drives merged in",
			entry)
	}

	var worker corev1.Node
	if err := apiClient.Get(context.Background(), client.ObjectKey{Name: opsTarget}, &worker); err != nil {
		t.Fatalf("reading the target worker: %v", err)
	}
	if worker.Labels[atlaskube.LabelStorageNodeSet] != opsCluster {
		t.Errorf("the target carries %v, want the cluster's storage-plane label", worker.Labels)
	}
}

// Preparing blocks on the published name rather than on pod readiness. The
// control plane resolves node_address itself, and readiness happens before the
// EndpointSlice is published, so a restart issued on readiness alone is aimed at
// a name that does not yet exist.
func TestPreparingWaitsForTheNameToResolveAndNotOnlyForThePod(t *testing.T) {
	config := aPerNodeConfig("PCI_ALLOWED=''\n")

	unpublished, _ := anOpsWorld(t, aControlPlane(),
		aWorker(opsTarget, true), config, aReadyStoragePod(opsTarget))
	done, err := unpublished.perform(context.Background(), aRelocation(), stepPreparing)
	if err != nil {
		t.Fatalf("preparing: %v", err)
	}
	if done {
		t.Error("the step finished against a name the control plane could not resolve")
	}

	published, _ := anOpsWorld(t, aControlPlane(), aWorker(opsTarget, true), config,
		aReadyStoragePod(opsTarget), aPublishedName(opsTarget))
	done, err = published.perform(context.Background(), aRelocation(), stepPreparing)
	if err != nil {
		t.Fatalf("preparing: %v", err)
	}
	if !done {
		t.Error("the step did not finish although the target's name is published")
	}
}

// The restart is aimed at the target's own name, and it is forced by default: a
// migration relocates a node that is still online, and the control plane refuses
// a non-forced restart of one that is not already offline.
func TestTheRelocationRestartIsAimedAtTheTargetAndForced(t *testing.T) {
	api := aControlPlane()
	r, _ := anOpsWorld(t, api, aWorker(opsTarget, true))

	done, err := r.perform(context.Background(), aRelocation(), stepRelocating)
	if err != nil {
		t.Fatalf("relocating: %v", err)
	}
	if done {
		t.Error("the step finished while the node was still reporting online")
	}
	if len(api.restarts) != 1 {
		t.Fatalf("%d restarts were issued, want one", len(api.restarts))
	}
	if api.restarts[0].NodeAddress != utils.StorageNodeSetAPIAddress(opsTarget, opsNamespace) {
		t.Errorf("the restart names %q, want the target's per-pod address",
			api.restarts[0].NodeAddress)
	}
	if !api.restarts[0].Force {
		t.Error("the restart is not forced, and the control plane refuses it against an online node")
	}
}

// An operation that states the flag means it, including to turn the default off.
func TestAStatedForceOutranksTheRelocationsDefault(t *testing.T) {
	api := aControlPlane()
	r, _ := anOpsWorld(t, api, aWorker(opsTarget, true))
	ops := aRelocation()
	ops.Spec.Force = ptr.To(false)

	if _, err := r.perform(context.Background(), ops, stepRelocating); err != nil {
		t.Fatalf("relocating: %v", err)
	}
	if api.restarts[0].Force {
		t.Error("the operation asked for an unforced restart and was given a forced one")
	}
}

// A node that has left online has begun its restart, which is the observation
// this step exists to make. There is nothing left to issue.
func TestTheRelocationIsOverWhenTheNodeHasLeftOnline(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusInRestart)
	r, _ := anOpsWorld(t, api, aWorker(opsTarget, true))

	done, err := r.perform(context.Background(), aRelocation(), stepRelocating)
	if err != nil {
		t.Fatalf("relocating: %v", err)
	}
	if !done {
		t.Error("the step did not finish although the node had left online")
	}
	if asked := api.asked("RestartNode"); asked != 0 {
		t.Errorf("a second restart was issued %d time(s) against a node already restarting", asked)
	}
}

// The wait for the node is over when it is online again, and not while it is on
// its way there.
func TestTheWaitForTheRelocatedNodeEndsWhenItIsBack(t *testing.T) {
	restarting, _ := anOpsWorld(t, aControlPlane().reporting(nodeStatusInRestart),
		aWorker(opsTarget, true))
	done, err := restarting.perform(context.Background(), aRelocation(), stepAwaitingNode)
	if err != nil {
		t.Fatalf("awaiting the node: %v", err)
	}
	if done {
		t.Error("the wait finished against a node still restarting")
	}

	back, _ := anOpsWorld(t, aControlPlane(), aWorker(opsTarget, true))
	done, err = back.perform(context.Background(), aRelocation(), stepAwaitingNode)
	if err != nil {
		t.Fatalf("awaiting the node: %v", err)
	}
	if !done {
		t.Error("the wait did not finish against a node that is online again")
	}
}

// The promote is separately guarded, which is what makes the negative predicate
// in Relocating tolerable: promoting into an in-flight restart leaves the
// relocated devices stuck.
func TestAPromoteIsRefusedWhileTheNodeIsNotBack(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusInRestart)
	r, _ := anOpsWorld(t, api, aWorker(opsTarget, true))

	done, err := r.perform(context.Background(), aRelocation(), stepPromoting)
	if err != nil {
		t.Fatalf("promoting: %v", err)
	}
	if done {
		t.Error("the step finished against a node that has not come back")
	}
	if asked := api.asked("Promote"); asked != 0 {
		t.Errorf("Promote was issued %d time(s) into an in-flight restart", asked)
	}
}

// The promote lands and the Kubernetes view follows it, never the other way
// round: re-pointing first would leave the object describing a relocation the
// control plane had not performed.
func TestThePromoteIsFollowedByTheTopologyRepoint(t *testing.T) {
	api := aControlPlane()
	r, apiClient := anOpsWorld(t, api, aWorker(opsTarget, true))
	ops := aRelocation()
	ops.Spec.Migrate.NewSsdPcie = []string{"0000:04:00.0"}

	done, err := r.perform(context.Background(), ops, stepPromoting)
	if err != nil {
		t.Fatalf("promoting: %v", err)
	}
	if !done {
		t.Error("the step did not finish although the promote landed")
	}
	if asked := api.asked("Promote"); asked != 1 {
		t.Errorf("Promote was issued %d time(s), want once", asked)
	}

	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	if node.Spec.WorkerNode != opsTarget {
		t.Errorf("the node still names worker %q, so Kubernetes and the control plane disagree",
			node.Spec.WorkerNode)
	}
	if len(node.Spec.Config.PcieAllowList) != 1 ||
		node.Spec.Config.PcieAllowList[0] != "0000:04:00.0" {
		t.Errorf("the allow list is %v, want the drives the migration bound on the target",
			node.Spec.Config.PcieAllowList)
	}
}

// A node whose object already names the target was promoted by an earlier pass.
// The re-point is the last thing the step does, so its presence is the record
// that the promote landed, and a second one must not be issued.
func TestAPromoteThatAlreadyLandedIsNotIssuedAgain(t *testing.T) {
	node := anOpsNode()
	node.Spec.WorkerNode = opsTarget
	api := aControlPlane()
	r, apiClient := anOpsWorld(t, api, aWorker(opsTarget, true))
	replaceNode(t, apiClient, node)

	done, err := r.migratePromote(context.Background(), aRelocation(), node,
		opsTarget, opsClusterID, opsNodeID)
	if err != nil {
		t.Fatalf("promoting: %v", err)
	}
	if !done {
		t.Error("the step did not finish against a node that has already been promoted")
	}
	if asked := api.asked("Promote"); asked != 0 {
		t.Errorf("Promote was issued %d time(s) against a node already on its target", asked)
	}
}

// The drives a migration bound are added to what the node already had, in the
// order somebody wrote them: the field is a user's, and one the operator
// rewrites should come back recognizable.
func TestTheBoundDrivesJoinTheListInTheOrderItWasWritten(t *testing.T) {
	merged := mergePCIAddresses(
		[]string{"0000:02:00.0", "0000:03:00.0"},
		[]string{"0000:03:00.0", "", "0000:04:00.0"})

	want := []string{"0000:02:00.0", "0000:03:00.0", "0000:04:00.0"}
	if len(merged) != len(want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
	for i := range want {
		if merged[i] != want[i] {
			t.Fatalf("merged = %v, want %v", merged, want)
		}
	}
}

// Nothing to add leaves the list exactly as it was.
func TestAMigrationThatBindsNoDriveLeavesTheListAlone(t *testing.T) {
	existing := []string{"0000:02:00.0"}
	if merged := mergePCIAddresses(existing, nil); len(merged) != 1 {
		t.Errorf("merged = %v, want the list untouched", merged)
	}
}

// A worker with no Ready condition at all has not reported one, which is not the
// same as having reported that it is Ready.
func TestAWorkerThatHasNotReportedIsNotReady(t *testing.T) {
	if workerReady(&corev1.Node{}) {
		t.Error("a worker with no conditions was read as Ready")
	}
	if !workerReady(aWorker(opsWorker, true)) {
		t.Error("a Ready worker was read as not Ready")
	}
}

// replaceNode swaps the fixture's node for one a case has rewritten.
func replaceNode(t *testing.T, apiClient client.Client, node *simplyblockv1alpha2.StorageNode) {
	t.Helper()
	existing := anOpsNode()
	if err := apiClient.Delete(context.Background(), existing); err != nil {
		t.Fatalf("clearing the fixture's node: %v", err)
	}
	status := *node.Status.DeepCopy()
	node.ResourceVersion = ""
	if err := apiClient.Create(context.Background(), node); err != nil {
		t.Fatalf("seeding the case's node: %v", err)
	}
	node.Status = status
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("seeding the case's node status: %v", err)
	}
}
