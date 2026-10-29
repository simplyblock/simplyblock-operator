// The reachability gate: a node on a network is not attached, and not online,
// until the CSI node plugins of the workers connect to its SPDK listener. These
// tests drive whole reconcile passes, so the phase the node is reported in is
// read from the object, not from the gate's own return value.

package node

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const gatedPort = 4426

// aGatedWorld is a node on one network whose SPDK pod holds the declared
// address, online in the control plane, with the host check in place. The
// listener port is the one the control plane reports, so a test that leaves it
// out is the node before the control plane has assigned one.
func aGatedWorld(t *testing.T, host *fakeHost, withPort bool) (*StorageNodeReconciler, client.Client) {
	t.Helper()
	r, apiClient := onANetwork(t, onDefinition(storageNetwork), anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.101")))
	if withPort {
		api := r.API.(*scriptedControlPlane)
		reading := api.nodes[opsNodeID]
		reading.NVMeOFPort = gatedPort
		api.nodes[opsNodeID] = reading
	}
	r.HostCheck = host
	return r, apiClient
}

func storageANetwork(t *testing.T, apiClient client.Client) simplyblockv1alpha2.NodeNetworkStatus {
	t.Helper()
	for _, n := range nodeRead(t, apiClient).Status.Network {
		if n.Network == storageNetwork {
			return n
		}
	}
	t.Fatal("the node has no status for the storage network")
	return simplyblockv1alpha2.NodeNetworkStatus{}
}

// settleAll runs passes and returns every event they raised.
func settleAll(t *testing.T, r *StorageNodeReconciler, passes int) []string {
	t.Helper()
	all := make([]string, 0, passes)
	for range passes {
		settle(t, r)
		all = append(all, recordedEvents(r)...)
	}
	return all
}

// I-64
func TestEveryLinkedWorkerReachingTheListenerAttachesTheNetworkAndBringsTheNodeOnline(t *testing.T) {
	host := &fakeHost{workers: []string{"worker-a", "worker-b"}}
	r, apiClient := aGatedWorld(t, host, true)

	got := settleAll(t, r, 3)

	n := storageANetwork(t, apiClient)
	if n.Phase != simplyblockv1alpha2.NodeNetworkPhaseAttached || n.HostReachable == nil || !*n.HostReachable {
		t.Fatalf("network = %+v, want attached and reachable", n)
	}
	want := []string{"worker-a 10.10.10.101:4426", "worker-b 10.10.10.101:4426"}
	if strings.Join(host.connects, ",") != strings.Join(want, ",") {
		t.Errorf("connections = %v, want one from each linked worker to the pod's address and the control plane's port", host.connects)
	}
	if count := recordedReason(got, NetworkAttached); count != 1 {
		t.Errorf("NetworkAttached was recorded %d times, want 1", count)
	}
	if phase := nodeRead(t, apiClient).Status.Phase; phase != simplyblockv1alpha2.StorageNodePhaseOnline {
		t.Errorf("node phase = %s, want Online once the gate passed", phase)
	}
}

// I-65
func TestAWorkerThatCannotReachTheListenerKeepsTheNetworkUnavailableAndTheNodeHeld(t *testing.T) {
	host := &fakeHost{workers: []string{"worker-a", "worker-b"}, refused: map[string]string{"worker-b": "connection timed out"}}
	r, apiClient := aGatedWorld(t, host, true)

	got := settleAll(t, r, 3)

	n := storageANetwork(t, apiClient)
	if n.Phase != simplyblockv1alpha2.NodeNetworkPhaseUnavailable || n.HostReachable == nil || *n.HostReachable {
		t.Fatalf("network = %+v, want unavailable and not reachable", n)
	}
	if count := recordedReason(got, DataNetworkHostUnreachable); count != 1 {
		t.Fatalf("DataNetworkHostUnreachable was recorded %d times, want 1 for the transition", count)
	}
	for _, e := range got {
		if !strings.Contains(e, DataNetworkHostUnreachable) {
			continue
		}
		if !strings.Contains(e, "worker-b") || !strings.Contains(e, "connection timed out") {
			t.Errorf("event %q, want it to name the worker and the reason", e)
		}
		for _, hint := range []string{"routes", "security group", "sibling"} {
			if !strings.Contains(e, hint) {
				t.Errorf("event %q, want it to point at %s", e, hint)
			}
		}
	}
	if recordedReason(got, NetworkAttached) != 0 {
		t.Error("NetworkAttached was recorded for a network a worker cannot reach")
	}
	if phase := nodeRead(t, apiClient).Status.Phase; phase != simplyblockv1alpha2.StorageNodePhaseProvisioning {
		t.Errorf("node phase = %s, want it held out of Online", phase)
	}
}

// The hold clears once the path is mended, with nothing else to do.
func TestTheNodeComesOnlineOnceTheWorkerCanReachTheListener(t *testing.T) {
	host := &fakeHost{refused: map[string]string{opsWorker: "connection timed out"}}
	r, apiClient := aGatedWorld(t, host, true)
	settleAll(t, r, 2)

	delete(host.refused, opsWorker)
	got := settleAll(t, r, 3)

	if n := storageANetwork(t, apiClient); n.Phase != simplyblockv1alpha2.NodeNetworkPhaseAttached || n.HostReachable == nil || !*n.HostReachable {
		t.Fatalf("network = %+v, want attached once the connection succeeds", n)
	}
	if recordedReason(got, NetworkAttached) != 1 {
		t.Errorf("NetworkAttached was recorded %d times, want 1", recordedReason(got, NetworkAttached))
	}
	if phase := nodeRead(t, apiClient).Status.Phase; phase != simplyblockv1alpha2.StorageNodePhaseOnline {
		t.Errorf("node phase = %s, want Online", phase)
	}
}

// I-68
func TestTheGateWaitsForTheControlPlaneToReportTheListenerPort(t *testing.T) {
	host := &fakeHost{}
	r, apiClient := aGatedWorld(t, host, false)

	got := settleAll(t, r, 2)

	if len(host.connects) != 0 {
		t.Errorf("connections = %v, want none before the control plane reports a port", host.connects)
	}
	if n := storageANetwork(t, apiClient); n.Phase != simplyblockv1alpha2.NodeNetworkPhasePending {
		t.Errorf("phase = %s, want Pending while the gate cannot run", n.Phase)
	}
	if recordedReason(got, DataNetworkHostUnreachable) != 0 {
		t.Error("a gate that did not run reported a worker unreachable")
	}
}

// I-71
func TestNoLinkedPluginKeepsTheNetworkPendingAndSaysSo(t *testing.T) {
	host := &fakeHost{noWorkers: true}
	r, apiClient := aGatedWorld(t, host, true)

	got := settleAll(t, r, 2)

	if n := storageANetwork(t, apiClient); n.Phase != simplyblockv1alpha2.NodeNetworkPhasePending || n.HostReachable != nil {
		t.Errorf("network = %+v, want Pending and unchecked", n)
	}
	if recordedReason(got, NetworkHostCheckUnavailable) == 0 {
		t.Error("no NetworkHostCheckUnavailable event for a node no plugin can check")
	}
	if phase := nodeRead(t, apiClient).Status.Phase; phase != simplyblockv1alpha2.StorageNodePhaseProvisioning {
		t.Errorf("node phase = %s, want it held out of Online", phase)
	}
}

func TestAReachableNetworkIsNotTestedAgain(t *testing.T) {
	host := &fakeHost{}
	r, _ := aGatedWorld(t, host, true)

	settleAll(t, r, 4)

	if len(host.connects) != 1 {
		t.Errorf("connections = %v, want one: a passed gate is not repeated on every pass", host.connects)
	}
}

// I-66
func TestTheGateRunsAgainWhenTheAttachIsLostAndRegained(t *testing.T) {
	host := &fakeHost{}
	r, apiClient := aGatedWorld(t, host, true)
	settleAll(t, r, 2)
	first := len(host.connects)

	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: opsNamespace, Name: "snode-spdk-pod-8080-abc"}
	if err := apiClient.Get(context.Background(), key, pod); err != nil {
		t.Fatal(err)
	}
	// A restarted pod has no network-status until Multus has attached it again.
	attached := pod.Annotations[NetworkStatusAnnotation]
	delete(pod.Annotations, NetworkStatusAnnotation)
	pod.Status.Phase = corev1.PodPending
	if err := apiClient.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	settleAll(t, r, 1)
	if n := storageANetwork(t, apiClient); n.HostReachable != nil {
		t.Fatalf("hostReachable = %v after the attach was lost, want it cleared", *n.HostReachable)
	}

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[NetworkStatusAnnotation] = attached
	pod.Status.Phase = corev1.PodRunning
	if err := apiClient.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	settleAll(t, r, 2)

	if len(host.connects) <= first {
		t.Errorf("connections = %v, want the gate to run again after the attach returned", host.connects)
	}
}

func TestAFailedCallIsRetriedAndNotRecordedAsUnreachable(t *testing.T) {
	host := &fakeHost{connectErr: context.DeadlineExceeded}
	r, apiClient := aGatedWorld(t, host, true)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}})
	if err == nil {
		t.Fatal("a call that failed was swallowed, want the pass to retry")
	}
	if n := storageANetwork(t, apiClient); n.Phase == simplyblockv1alpha2.NodeNetworkPhaseUnavailable || n.HostReachable != nil {
		t.Errorf("network = %+v, want a failed call recorded as neither reached nor unreachable", n)
	}
}
