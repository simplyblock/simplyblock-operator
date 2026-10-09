// Tests for withdrawing an export's address while its metadata server pod is
// going away, and for the connection-tracking flush that follows a move
// (design-pnfs-mds-vm.md §8.1). They run against the fake client, since what is
// checked is what the reconciler writes and whom it asks to forget a flow.

package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// fakeFlows records the addresses the reconciler asked the nodes to forget.
type fakeFlows struct{ forgotten []string }

func (f *fakeFlows) Forget(ip string) { f.forgotten = append(f.forgotten, ip) }

func exportEndpoints(t *testing.T, cl client.Client) []discoveryv1.Endpoint {
	t.Helper()
	var eps discoveryv1.EndpointSlice
	key := client.ObjectKey{Name: utils.NFSExportEndpointSliceName(testExportName), Namespace: testExportNS}
	if err := cl.Get(context.Background(), key, &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	return eps.Endpoints
}

// terminatingMDSPod is the pod that assembled readyPodHosted, being deleted.
// The fake client keeps an object with a deletion timestamp only while it
// carries a finalizer.
func terminatingMDSPod() *corev1.Pod {
	p := runningMDSPod(testMDSPodUID, testMDSPodIP)
	now := metav1.Now()
	p.DeletionTimestamp = &now
	p.Finalizers = []string{"test.simplyblock.io/hold"}
	return p
}

func newWithdrawReconciler(t *testing.T, objects ...client.Object) (
	*NFSExportReconciler, client.Client, *reasonRecorder, *fakeFlows,
) {
	t.Helper()
	r, cl, events := newResyncReconciler(t, &fakeAssembler{}, objects...)
	flows := &fakeFlows{}
	r.Flows = flows
	// The Service and EndpointSlice exist from the binding.
	if _, err := r.reconcileExportService(context.Background(), loadExport(t, cl), testMDSPodIP); err != nil {
		t.Fatalf("creating the Service: %v", err)
	}
	return r, cl, events, flows
}

// Regression: 2026-10-09-pnfs-mds-conntrack-pinning (run pnfs-1791575321).
// Clients reconnected within a second of the MDS pod's deletion, and kube-proxy
// sent them to the dead pod for the 5 s until the replacement had an address.
// Every SYN retry kept that translation alive in conntrack for two minutes. A
// terminating pod's address leaves the EndpointSlice at once, so new
// connections are refused rather than sent to it.
func TestATerminatingMDSPodsAddressIsWithdrawn(t *testing.T) {
	r, cl, events, flows := newWithdrawReconciler(t, readyPodHosted(), terminatingMDSPod())

	reconcileExport(t, r)

	if got := exportEndpoints(t, cl); len(got) != 0 {
		t.Errorf("EndpointSlice endpoints = %+v, want none while the pod terminates", got)
	}
	got := loadExport(t, cl)
	if got.Status.MDSNodeIP != "" {
		t.Errorf("mdsNodeIP = %q, want it cleared", got.Status.MDSNodeIP)
	}
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseReady {
		t.Errorf("phase = %q, want Ready: a pod restart is not a new assembly", got.Status.Phase)
	}
	if !slices.Contains(events.reasons, "MDSAddressWithdrawn") {
		t.Errorf("events = %v, want MDSAddressWithdrawn", events.reasons)
	}
	if !slices.Equal(flows.forgotten, []string{testMDSPodIP}) {
		t.Errorf("forgotten = %v, want the old pod IP %s", flows.forgotten, testMDSPodIP)
	}
}

func TestAGoneMDSPodsAddressIsWithdrawn(t *testing.T) {
	r, cl, events, flows := newWithdrawReconciler(t, readyPodHosted())

	reconcileExport(t, r)

	if got := exportEndpoints(t, cl); len(got) != 0 {
		t.Errorf("EndpointSlice endpoints = %+v, want none with the pod gone", got)
	}
	if !slices.Contains(events.reasons, "MDSAddressWithdrawn") {
		t.Errorf("events = %v, want MDSAddressWithdrawn", events.reasons)
	}
	if !slices.Equal(flows.forgotten, []string{testMDSPodIP}) {
		t.Errorf("forgotten = %v, want the old pod IP %s", flows.forgotten, testMDSPodIP)
	}
}

// A withdrawn address stays withdrawn: a second pass over the same terminating
// pod writes nothing and asks for no second flush.
func TestAWithdrawnAddressIsWithdrawnOnce(t *testing.T) {
	r, _, events, flows := newWithdrawReconciler(t, readyPodHosted(), terminatingMDSPod())

	reconcileExport(t, r)
	reconcileExport(t, r)

	n := 0
	for _, reason := range events.reasons {
		if reason == "MDSAddressWithdrawn" {
			n++
		}
	}
	if n != 1 || len(flows.forgotten) != 1 {
		t.Errorf("withdrawn %d time(s), forgotten %v: want once", n, flows.forgotten)
	}
}

// Once the replacement has an address the export points at it, and the nodes
// are asked once more to forget the old one: a connection that reached the old
// address before the withdrawal took effect is pinned to it too.
func TestTheReplacementsAddressFollowsAWithdrawal(t *testing.T) {
	const newIP = "10.244.7.4"
	r, cl, events, flows := newWithdrawReconciler(t, readyPodHosted(), terminatingMDSPod())
	reconcileExport(t, r)

	// The StatefulSet replaces the pod under the same name.
	if err := cl.Delete(context.Background(), terminatingMDSPod()); err != nil {
		t.Fatal(err)
	}
	old := &corev1.Pod{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testOperatorNS, Name: testMDSPod}, old); err == nil {
		old.Finalizers = nil
		if err := cl.Update(context.Background(), old); err != nil {
			t.Fatal(err)
		}
	}
	if err := cl.Create(context.Background(), runningMDSPod(restartedPodUID, newIP)); err != nil {
		t.Fatal(err)
	}
	reconcileExport(t, r)

	got := exportEndpoints(t, cl)
	if len(got) != 1 || got[0].Addresses[0] != newIP {
		t.Errorf("EndpointSlice endpoints = %+v, want the new pod IP %s", got, newIP)
	}
	if !slices.Contains(events.reasons, "MDSAddressChanged") {
		t.Errorf("events = %v, want MDSAddressChanged", events.reasons)
	}
	if !slices.Equal(flows.forgotten, []string{testMDSPodIP, testMDSPodIP}) {
		t.Errorf("forgotten = %v, want the old pod IP at the withdrawal and again at the move", flows.forgotten)
	}
}

// A move the reconciler saw only after it happened (the old pod already
// replaced when it looked) still has the nodes forget the old address.
func TestAMoveWithoutAWithdrawalForgetsTheOldAddress(t *testing.T) {
	const newIP = "10.244.7.4"
	r, _, _, flows := newWithdrawReconciler(t, readyPodHosted(), runningMDSPod(restartedPodUID, newIP))

	reconcileExport(t, r)

	if !slices.Equal(flows.forgotten, []string{testMDSPodIP}) {
		t.Errorf("forgotten = %v, want the old pod IP %s", flows.forgotten, testMDSPodIP)
	}
}
