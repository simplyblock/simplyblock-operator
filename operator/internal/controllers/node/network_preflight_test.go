// The preflight gate: a node on a network is held at CheckingNetwork until a
// probe pod, attached the way the SPDK pod will be, has shown that the
// definition exists, the pod got the declared address, no other host holds it,
// and the peers answer. The probe is gone before the node-add call is made.

package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/multus"
	"github.com/simplyblock/simplyblock-operator/internal/netprobe"
)

const (
	probeImage    = "example.test/operator:v1"
	storageADef   = "storage-a-def"
	passingResult = `{"interfaces":[{"name":"data0","mtu":1500}],"duplicates":[{"interface":"data0","address":"10.10.10.101","replies":0}],"peers":[]}`
)

func theDefinition() *unstructured.Unstructured {
	nad := multus.NewDefinition()
	nad.SetNamespace(opsNamespace)
	nad.SetName(storageADef)
	return nad
}

// aProbeWorld is a node on one network whose definition exists, with the probe
// image set.
func aProbeWorld(t *testing.T, objects ...client.Object) (*StorageNodeReconciler, client.Client) {
	t.Helper()
	r, apiClient := onANetwork(t, viaDefinition("storage-a"), append([]client.Object{theDefinition()}, objects...)...)
	r.ProbeImage = probeImage
	unassign(t, apiClient)
	return r, apiClient
}

// unassign takes back the address onANetwork recorded, which is the node as it is
// when it first reaches CheckingNetwork: nothing chosen yet.
func unassign(t *testing.T, apiClient client.Client) {
	t.Helper()
	node := nodeRead(t, apiClient)
	node.Status.Network = nil
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
}

// check runs the gate once against what the API server holds.
func check(t *testing.T, r *StorageNodeReconciler) (nodeStep, bool, error) {
	t.Helper()
	node, cluster := nodeAndClusterOf(t, r)
	return r.checkNetwork(context.Background(), node, cluster)
}

func probeName() string { return netprobe.Name(opsNodeName, netprobe.KindGuest) }

func probeOf(t *testing.T, apiClient client.Client) (*corev1.Pod, bool) {
	t.Helper()
	var pod corev1.Pod
	err := apiClient.Get(context.Background(), client.ObjectKey{Namespace: opsNamespace, Name: probeName()}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return &pod, true
}

// finishProbe is the probe pod having run: it holds what network-status says and
// reports what its termination message says.
func finishProbe(t *testing.T, apiClient client.Client, networkStatus, message string) {
	t.Helper()
	pod, found := probeOf(t, apiClient)
	if !found {
		t.Fatal("no probe pod to finish")
	}
	pod.Annotations[NetworkStatusAnnotation] = networkStatus
	if err := apiClient.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	// The checks have exited and the holder keeps the pod, and its network, running.
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: netprobe.CheckContainer, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: message},
		}},
		{Name: netprobe.HoldContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}
	if err := apiClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func blockedReason(t *testing.T, err error) string {
	t.Helper()
	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the node held with a reason", err)
	}
	return blocked.reason
}

// I-14: the probe is created, and once it passes the node moves on with the probe
// gone and the add carrying the attachments.
func TestAPassingProbeLetsTheNodeAdvanceAndIsGoneBeforeTheAdd(t *testing.T) {
	r, apiClient := aProbeWorld(t)

	if _, done, err := check(t, r); err != nil || done {
		t.Fatalf("first pass: done=%v err=%v, want the probe created and the node waiting", done, err)
	}
	pod, found := probeOf(t, apiClient)
	if !found {
		t.Fatal("the first pass created no probe pod")
	}
	if pod.Spec.Containers[0].Image != probeImage {
		t.Errorf("probe image = %q, want the operator's", pod.Spec.Containers[0].Image)
	}

	finishProbe(t, apiClient, attachedAs("10.10.10.101"), passingResult)

	// The guest holds its address while the host probe pings it.
	if _, done, err := check(t, r); err != nil || done {
		t.Fatalf("after the guest passed: done=%v err=%v, want the host probe started", done, err)
	}
	if _, found := probeOf(t, apiClient); !found {
		t.Fatal("the guest probe was removed before the host probe could reach it")
	}
	finishHostProbe(t, apiClient, `{"targets":[{"address":"10.10.10.101","ok":true}]}`)

	next, done, err := check(t, r)
	if err != nil || !done || next != stepAwaitingSlot {
		t.Fatalf("final pass: next=%s done=%v err=%v, want AwaitingSlot", next, done, err)
	}

	// I-22: the node-add call is not made while a probe still exists.
	if _, found := probeOf(t, apiClient); found {
		t.Fatal("the probe pod was left behind when the node advanced")
	}
	if _, found := hostProbeOf(t, apiClient); found {
		t.Fatal("the host probe was left behind when the node advanced")
	}
}

// I-15
func TestAClusterWithoutTheDefinitionKindHoldsTheNode(t *testing.T) {
	r, _ := aProbeWorld(t)
	base := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == multus.DefinitionGVK {
				return &meta.NoKindMatchError{GroupKind: multus.DefinitionGVK.GroupKind()}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	_, _, err := check(t, r)

	if reason := blockedReason(t, err); reason != NetworkAttachmentUnsupported {
		t.Fatalf("reason = %s, want %s", reason, NetworkAttachmentUnsupported)
	}
}

// I-16 and I-23: a definition that is not there holds the node, and creating it
// is all it takes for the next pass to proceed.
func TestAMissingDefinitionHoldsTheNodeUntilItIsCreated(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if err := apiClient.Delete(context.Background(), theDefinition()); err != nil {
		t.Fatal(err)
	}

	_, done, err := check(t, r)
	if done || blockedReason(t, err) != NetworkAttachmentNotFound {
		t.Fatalf("done=%v err=%v, want the node held for NetworkAttachmentNotFound", done, err)
	}
	if _, found := probeOf(t, apiClient); found {
		t.Fatal("a probe was created for a definition that does not exist")
	}

	if err := apiClient.Create(context.Background(), theDefinition()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := check(t, r); err != nil {
		t.Fatalf("after the definition exists: %v", err)
	}
	if _, found := probeOf(t, apiClient); !found {
		t.Fatal("the next pass did not start the probe")
	}
}

// I-17: a probe still not running after the timeout holds the node, naming the
// pod so the administrator can describe it, and is removed so the next pass starts
// a fresh one. The operator reads no events: the pod's own events say why.
func TestAProbeThatCannotAttachHoldsTheNodeNamingThePod(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	pod, _ := probeOf(t, apiClient)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * probeStartTimeout))
	if err := apiClient.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodPending
	if err := apiClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}

	_, done, err := check(t, r)

	if done || blockedReason(t, err) != NetworkAttachmentFailed {
		t.Fatalf("done=%v err=%v, want the node held for NetworkAttachmentFailed", done, err)
	}
	var blocked *blockedStepError
	errors.As(err, &blocked)
	if !strings.Contains(blocked.message, probeName()) {
		t.Errorf("message = %q, want it to name the probe pod %s", blocked.message, probeName())
	}
	if _, found := probeOf(t, apiClient); found {
		t.Error("the stuck probe was left holding the address")
	}
}

// A probe inside the timeout is only slow: kubelet retries the sandbox, and a first
// attempt that races Multus is not a fault.
func TestAProbeInsideTheStartTimeoutIsLeftAlone(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	pod, _ := probeOf(t, apiClient)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-probeStartTimeout + time.Second))
	if err := apiClient.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}

	if _, done, err := check(t, r); err != nil || done {
		t.Fatalf("done=%v err=%v, want the node waiting", done, err)
	}
	if _, found := probeOf(t, apiClient); !found {
		t.Error("a young probe was removed")
	}
}

// A finished probe that holds another address, or whose peer is silent, holds the
// node for the matching reason.
func TestAFailedProbeHoldsTheNodeForItsCause(t *testing.T) {
	for name, tc := range map[string]struct {
		status, result, reason string
	}{
		"another address": {attachedAs("10.10.10.77"), passingResult, NetworkAddressMismatch},
		"a silent peer": {attachedAs("10.10.10.101"),
			`{"interfaces":[{"name":"data0","mtu":1500}],"duplicates":[{"interface":"data0","address":"10.10.10.101","replies":0}],"peers":[{"interface":"data0","peer":"10.10.10.102","ok":false}]}`,
			DataNetworkUnreachable},
	} {
		t.Run(name, func(t *testing.T) {
			r, apiClient := aProbeWorld(t, anOnlinePeer("peer-node", "10.10.10.102/24"))
			if _, _, err := check(t, r); err != nil {
				t.Fatal(err)
			}
			if pod, _ := probeOf(t, apiClient); !envHas(pod.Spec.Containers[0].Env, "PEERS", "data0=10.10.10.102") {
				t.Fatalf("probe env = %+v, want the peer on the network", pod.Spec.Containers[0].Env)
			}
			finishProbe(t, apiClient, tc.status, tc.result)

			_, done, err := check(t, r)

			if done || blockedReason(t, err) != tc.reason {
				t.Fatalf("done=%v err=%v, want the node held for %s", done, err, tc.reason)
			}
		})
	}
}

// I-19: an address another host answers for is turned away and the node takes the
// next one of the pool, which holds nobody.
func TestAnAddressAnotherHostAnswersForIsTurnedAwayAndTheNextIsTried(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	finishProbe(t, apiClient, attachedAs("10.10.10.101"),
		`{"interfaces":[{"name":"data0","mtu":1500}],"duplicates":[{"interface":"data0","address":"10.10.10.101","replies":1}],"peers":[]}`)

	next, done, err := check(t, r)

	if err != nil || done || next != stepCheckingNetwork {
		t.Fatalf("next=%s done=%v err=%v, want the node to stay at the step and not be held", next, done, err)
	}
	status := nodeRead(t, apiClient).Status.Network
	if len(status) != 1 || status[0].Assigned != "" || len(status[0].Rejected) != 1 || status[0].Rejected[0] != "10.10.10.101/24" {
		t.Fatalf("status.network = %+v, want 10.10.10.101/24 rejected and nothing assigned", status)
	}
	if _, found := probeOf(t, apiClient); found {
		t.Fatal("the probe that found the duplicate was not removed")
	}

	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	if status := nodeRead(t, apiClient).Status.Network; status[0].Assigned != "10.10.10.102/24" {
		t.Fatalf("assigned = %q, want the next address of the pool", status[0].Assigned)
	}
}

// I-24: the probe has a name that follows from the node, so a pass that runs
// again, as after an operator restart, finds it and creates no second.
func TestARepeatedPassNeverCreatesASecondProbe(t *testing.T) {
	r, apiClient := aProbeWorld(t)

	for range 3 {
		if _, _, err := check(t, r); err != nil {
			t.Fatal(err)
		}
	}

	var pods corev1.PodList
	if err := apiClient.List(context.Background(), &pods, client.InNamespace(opsNamespace),
		client.MatchingLabels{netprobe.KindLabel: netprobe.KindGuest}); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("%d probe pods, want one", len(pods.Items))
	}
}

// The gate belongs to a cluster that declares a network: another one creates no
// probe at all.
func TestAClusterWithoutANetworkRunsNoProbe(t *testing.T) {
	r, apiClient := aSteadyNode(t, aControlPlane())
	r.ProbeImage = probeImage

	next, done, err := r.checkNetwork(context.Background(), anUnprovisionedNode(stepCheckingNetwork), anOpsCluster())

	if err != nil || !done || next != stepAwaitingSlot {
		t.Fatalf("next=%s done=%v err=%v, want AwaitingSlot at once", next, done, err)
	}
	if _, found := probeOf(t, apiClient); found {
		t.Fatal("a cluster with no network created a probe")
	}
}

// I-22: Posting makes no node-add call while a probe still holds the address.
func TestPostingWaitsForTheProbeToBeGone(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	api := r.API.(*scriptedControlPlane)
	node, cluster := nodeAndClusterOf(t, r)

	_, done, err := r.performNodeStep(context.Background(), node, cluster, stepPosting)
	if err != nil || done {
		t.Fatalf("done=%v err=%v, want Posting to wait for the probe", done, err)
	}
	if api.asked("AddNode") != 0 {
		t.Fatal("the node-add call was made while the probe held the address")
	}
	if _, found := probeOf(t, apiClient); found {
		t.Fatal("Posting did not remove the probe")
	}

	if _, done, err := r.performNodeStep(context.Background(), node, cluster, stepPosting); err != nil || !done {
		t.Fatalf("done=%v err=%v, want the add made once the probe is gone", done, err)
	}
	if api.asked("AddNode") != 1 {
		t.Fatalf("AddNode asked %d times, want once", api.asked("AddNode"))
	}
}

func aPeerOnTheNetwork(name, address string) *simplyblockv1alpha2.StorageNode {
	peer := anOpsNode()
	peer.Name = name
	peer.UID = ""
	peer.ResourceVersion = ""
	peer.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
		Network: "storage-a", Phase: simplyblockv1alpha2.NodeNetworkPhaseAttached, Assigned: address,
	}}
	return peer
}

func envHas(env []corev1.EnvVar, name, value string) bool {
	for _, e := range env {
		if e.Name == name && e.Value == value {
			return true
		}
	}
	return false
}

func hostProbeName() string { return netprobe.Name(opsNodeName, netprobe.KindHost) }

func hostProbeOf(t *testing.T, apiClient client.Client) (*corev1.Pod, bool) {
	t.Helper()
	var pod corev1.Pod
	err := apiClient.Get(context.Background(), client.ObjectKey{Namespace: opsNamespace, Name: hostProbeName()}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return &pod, true
}

func finishHostProbe(t *testing.T, apiClient client.Client, message string) {
	t.Helper()
	pod, found := hostProbeOf(t, apiClient)
	if !found {
		t.Fatal("no host probe to finish")
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  netprobe.CheckContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
	}}
	if err := apiClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

// passTheGuest runs the gate until the guest probe has passed and the host probe
// exists.
func passTheGuest(t *testing.T, r *StorageNodeReconciler, apiClient client.Client) {
	t.Helper()
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	finishProbe(t, apiClient, attachedAs("10.10.10.101"), passingResult)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	if _, found := hostProbeOf(t, apiClient); !found {
		t.Fatal("the host probe was not started once the guest passed")
	}
}

// I-21: a host that cannot reach the address is warned about and the node goes
// on. Either way the verdict is recorded, and the host probe is gone.
func TestTheHostVerdictIsRecordedAndTheNodeProceeds(t *testing.T) {
	for _, reachable := range []bool{true, false} {
		t.Run(fmt.Sprintf("reachable=%v", reachable), func(t *testing.T) {
			r, apiClient := aProbeWorld(t)
			passTheGuest(t, r, apiClient)
			finishHostProbe(t, apiClient, fmt.Sprintf(`{"targets":[{"address":"10.10.10.101","ok":%v}]}`, reachable))

			next, done, err := check(t, r)

			if err != nil || !done || next != stepAwaitingSlot {
				t.Fatalf("next=%s done=%v err=%v, want the node to proceed", next, done, err)
			}
			status := nodeRead(t, apiClient).Status.Network
			if len(status) != 1 || status[0].HostReachable == nil || *status[0].HostReachable != reachable {
				t.Fatalf("status.network = %+v, want hostReachable %v", status, reachable)
			}
			warned := recordedReason(recordedEvents(r), DataNetworkHostUnreachable)
			if (warned == 1) == reachable || warned > 1 {
				t.Errorf("DataNetworkHostUnreachable emitted %d times for reachable=%v", warned, reachable)
			}
			if _, found := hostProbeOf(t, apiClient); found {
				t.Error("the host probe was left behind")
			}
		})
	}
}

// A host probe that gives no verdict does not hold the node: its result is a
// warning, and the pod's own deadline ends it.
func TestAHostProbeWithNoVerdictDoesNotHoldTheNode(t *testing.T) {
	for name, giveNoVerdict := range map[string]func(*testing.T, client.Client){
		"never finishes": func(t *testing.T, apiClient client.Client) {
			pod, _ := hostProbeOf(t, apiClient)
			pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
			if err := apiClient.Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
		},
		"result cannot be read": func(t *testing.T, apiClient client.Client) { finishHostProbe(t, apiClient, "not json") },
	} {
		t.Run(name, func(t *testing.T) {
			r, apiClient := aProbeWorld(t)
			passTheGuest(t, r, apiClient)
			giveNoVerdict(t, apiClient)

			next, done, err := check(t, r)

			if err != nil || !done || next != stepAwaitingSlot {
				t.Fatalf("next=%s done=%v err=%v, want the node to proceed", next, done, err)
			}
			for _, n := range nodeRead(t, apiClient).Status.Network {
				if n.HostReachable != nil {
					t.Fatalf("status.network = %+v, want no host reachability recorded", n)
				}
			}
			if _, found := hostProbeOf(t, apiClient); found {
				t.Error("the host probe was left behind")
			}
		})
	}
}

func anOnlinePeer(name, address string) *simplyblockv1alpha2.StorageNode {
	peer := aPeerOnTheNetwork(name, address)
	peer.Status.Phase = simplyblockv1alpha2.StorageNodePhaseOnline
	return peer
}

// A peer whose node has not been added has nothing listening on its declared
// address, so asking it to answer would hold the first node of a cluster forever.
func TestAPeerThatIsNotOnlineIsNotAskedToAnswer(t *testing.T) {
	r, apiClient := aProbeWorld(t, aPeerOnTheNetwork("peer-node", "10.10.10.102/24"))
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}

	pod, _ := probeOf(t, apiClient)
	if env := pod.Spec.Containers[0].Env; envHas(env, "PEERS", "data0=10.10.10.102") {
		t.Fatalf("probe env = %+v, want no peer that is not online", env)
	}
}
