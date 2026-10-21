// Tests of the takeover's Kubernetes side: the reconciler of the MDS
// StatefulSet, the EndpointSlice gateway, the exports adapter, the event sink,
// the gate that holds a binding until the active serves, and the host NQN of
// each ordinal: O-16, O-30, O-43, O-07 as the operator runs it, and the roles
// of design-pnfs-mds-standby.md §7 that need a fake client.
//
// The decisions are tested in internal/mdsfailover with fakes of every
// interface. What is checked here is that the adapters do what those
// interfaces promise against the objects the operator really writes.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	"github.com/simplyblock/simplyblock-operator/internal/mdsfailover"
	"github.com/simplyblock/simplyblock-operator/internal/mdsstate"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	failoverSTS   = "simplyblock-pnfs-mds-0f2ac1d3"
	failoverPod0  = failoverSTS + "-0"
	failoverPod1  = failoverSTS + "-1"
	failoverState = testExportClusterID + ":pool-a:3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
)

// objectRecorder keeps who an event was about, beside its reason.
type objectRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct{ kind, name, eventType, reason, note string }

func (r *objectRecorder) Eventf(regarding runtime.Object, _ runtime.Object, eventType, reason, _, note string, args ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	obj, _ := regarding.(client.Object)
	kind := fmt.Sprintf("%T", regarding)
	r.events = append(r.events, recordedEvent{kind, obj.GetName(), eventType, reason, fmt.Sprintf(note, args...)})
}

func (r *objectRecorder) about(reason string) []recordedEvent {
	var out []recordedEvent
	for _, e := range r.events {
		if e.reason == reason {
			out = append(out, e)
		}
	}
	return out
}

// fakeGuests is the link to the guests: sessions, roles, and the promotions it
// was asked for.
type fakeGuests struct {
	sessions map[string]bool
	roles    map[string]nfsexportrpc.Description
	promotes []nfsexportrpc.PromoteRequest
	onPod    []string
}

func newFakeGuests() *fakeGuests {
	return &fakeGuests{
		sessions: map[string]bool{failoverPod0: true, failoverPod1: true},
		roles: map[string]nfsexportrpc.Description{
			failoverPod0: {Role: nfsexportrpc.RoleStandby}, failoverPod1: {Role: nfsexportrpc.RoleStandby},
		},
	}
}

func (g *fakeGuests) HasSession(pod string) bool { return g.sessions[pod] }

func (g *fakeGuests) Describe(_ context.Context, pod string) (nfsexportrpc.Description, error) {
	if !g.sessions[pod] {
		return nfsexportrpc.Description{}, fmt.Errorf("no session for %s", pod)
	}
	return g.roles[pod], nil
}

func (g *fakeGuests) Promote(
	_ context.Context, pod string, req nfsexportrpc.PromoteRequest,
) (nfsexportrpc.PromoteResult, error) {
	g.promotes = append(g.promotes, req)
	g.onPod = append(g.onPod, pod)
	g.roles[pod] = nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: req.Epoch, Key: req.Key}
	return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteActive}, nil
}

func failoverStatefulSet(annotations map[string]string) *appsv1.StatefulSet {
	annotations = mapsWith(annotations, mdsfailover.AnnotationStateVolume, failoverState)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: failoverSTS, Namespace: testOperatorNS, UID: testMDSStatefulSetUID,
			Labels:      map[string]string{mdsfailover.ClusterLabel: testExportClusterID},
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "storage.simplyblock.io/v1alpha2", Kind: "SimplyblockDriver", Name: "simplyblock",
				UID: "driver-uid", Controller: ptr.To(true),
			}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "mds"}},
		},
	}
}

func mapsWith(m map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for key, val := range m {
		out[key] = val
	}
	return out
}

func failoverPodNamed(name, node, uid, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testOperatorNS, UID: types.UID(uid), Labels: map[string]string{"app": "mds"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "StatefulSet", Name: failoverSTS,
				UID: testMDSStatefulSetUID, Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			PodIP:             ip,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: mdsfailover.ContainerName, ContainerID: "containerd://" + uid}},
		},
	}
}

func failoverObjects(sts *appsv1.StatefulSet, extra ...client.Object) []client.Object {
	objects := []client.Object{
		sts, mdsDriver(),
		failoverPodNamed(failoverPod0, "n0", "uid-0", "10.244.0.10"),
		failoverPodNamed(failoverPod1, "n1", "uid-1", "10.244.1.10"),
		kubeNode("n0", "192.168.10.80"), kubeNode("n1", "192.168.10.81"),
	}
	return append(objects, extra...)
}

type failoverWorld struct {
	r      *MDSFailoverReconciler
	cl     client.Client
	guests *fakeGuests
	flows  *fakeFlows
	events *objectRecorder
}

func newFailoverWorld(t *testing.T, objects ...client.Object) *failoverWorld {
	t.Helper()
	scheme := newTestScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme, appsv1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{&simplyblockv1alpha2.NFSExport{}}, objects...)
	events := &objectRecorder{}
	flows := &fakeFlows{}
	guests := newFakeGuests()
	exports := &NFSExportReconciler{
		Client: cl, Scheme: scheme, Recorder: events, OperatorNamespace: testOperatorNS, Flows: flows,
	}
	return &failoverWorld{
		r: &MDSFailoverReconciler{
			Client: cl, Scheme: scheme, Recorder: events, Guests: guests, Exports: exports,
			OperatorNamespace: testOperatorNS, Clock: instantClock{},
		},
		cl: cl, guests: guests, flows: flows, events: events,
	}
}

// instantClock never waits, so a retry of a read costs a test nothing.
type instantClock struct{}

func (instantClock) Now() time.Time                             { return time.Now() }
func (instantClock) Sleep(context.Context, time.Duration) error { return nil }

func (w *failoverWorld) reconcile(t *testing.T) reconcile.Result {
	t.Helper()
	res, err := w.r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: testOperatorNS, Name: failoverSTS},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (w *failoverWorld) record(t *testing.T) mdsfailover.Record {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := w.cl.Get(context.Background(), client.ObjectKey{Namespace: testOperatorNS, Name: failoverSTS}, &sts); err != nil {
		t.Fatalf("reading the StatefulSet: %v", err)
	}
	rec, err := mdsfailover.ParseRecord(sts.Annotations)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	return rec
}

func TestO07TheFirstExportRecordsEpochOneAndPromotesThroughTheReconciler(t *testing.T) {
	w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(nil), testExport(nil))...)

	w.reconcile(t)

	rec := w.record(t)
	if rec.Active != failoverPod0 || rec.Epoch != 1 || rec.Instance != "uid-0/containerd://uid-0" {
		t.Fatalf("record = %+v, want the lowest ordinal at epoch 1 with its boot", rec)
	}
	if len(w.guests.promotes) != 1 || w.guests.onPod[0] != failoverPod0 {
		t.Fatalf("promotes = %d on %v, want one on %s", len(w.guests.promotes), w.guests.onPod, failoverPod0)
	}
	want, _ := nfsexport.MDSReservationKey(testMDSStatefulSetUID, 1)
	req := w.guests.promotes[0]
	if req.Key != want || req.HostNQN != wantNQN(t, 0) || req.StatefulSetUID != testMDSStatefulSetUID {
		t.Errorf("request = %+v, want the key of epoch 1 and the host NQN of ordinal 0", req)
	}
	if req.StateVolume.ClusterID != testExportClusterID || req.StateVolume.PoolID != "pool-a" {
		t.Errorf("state volume = %+v, want the recorded handle", req.StateVolume)
	}
}

func TestAReconcileOfAStatefulSetThatIsNotAnMDSOrIsGoneDoesNothing(t *testing.T) {
	w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(nil), testExport(nil))...)
	other := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: testOperatorNS}}
	if err := w.cl.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unrelated", "missing"} {
		res, err := w.r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: testOperatorNS, Name: name},
		})
		if err != nil || res.RequeueAfter != 0 {
			t.Errorf("%s: %+v, %v, want nothing", name, res, err)
		}
	}
	if len(w.guests.promotes) != 0 {
		t.Error("promoted on a StatefulSet that is not a metadata server")
	}
}

func boundExportOn(pod string, phase simplyblockv1alpha2.NFSExportPhase) *simplyblockv1alpha2.NFSExport {
	e := testExport(bound(phase))
	e.Status.MDSPodName = pod
	e.Status.AllowedClients = []string{testNodeIP}
	return e
}

func TestTheTakeoverHandsEachExportToTheNewActiveAndMovesTheEndpoint(t *testing.T) {
	record := map[string]string{
		mdsfailover.AnnotationActive: failoverPod0, mdsfailover.AnnotationActiveInstance: "uid-0/containerd://uid-0",
		mdsfailover.AnnotationEpoch: "1",
	}
	old := failoverPodNamed(failoverPod0, "n0", "uid-0", "10.244.0.10")
	now := metav1.Now()
	old.DeletionTimestamp = &now
	old.Finalizers = []string{"test.simplyblock.io/hold"}
	objects := failoverObjects(failoverStatefulSet(record), boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseReady))
	objects[2] = old
	w := newFailoverWorld(t, objects...)
	w.guests.roles[failoverPod0] = nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: 1}
	w.guests.sessions[failoverPod0] = false

	w.reconcile(t)

	if rec := w.record(t); rec.Active != failoverPod1 || rec.Epoch != 2 {
		t.Fatalf("record = %+v, want mds-1 at epoch 2", rec)
	}
	export := loadExport(t, w.cl)
	if export.Status.MDSPodName != failoverPod1 {
		t.Errorf("export mdsPodName = %q, want it handed to %s", export.Status.MDSPodName, failoverPod1)
	}
	var eps discoveryv1.EndpointSlice
	key := client.ObjectKey{Namespace: testOperatorNS, Name: utils.MDSEndpointSliceName(utils.MDSServiceName(failoverSTS))}
	if err := w.cl.Get(context.Background(), key, &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	if len(eps.Endpoints) != 1 || eps.Endpoints[0].Addresses[0] != "10.244.1.10" {
		t.Errorf("endpoints = %+v, want the new active's pod IP only", eps.Endpoints)
	}
}

// Both ordinals use one Service and one EndpointSlice: the name comes from the
// StatefulSet, so a takeover from ordinal 0 to ordinal 1 rewrites the slice
// that exists.
func TestO43TheServiceAndSliceAreNamedFromTheStatefulSetForEveryOrdinal(t *testing.T) {
	w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(nil))...)
	gw := &mdsGateway{r: w.r.Exports}

	if err := gw.Serve(context.Background(), failoverPod0, "10.244.0.10"); err != nil {
		t.Fatalf("Serve ordinal 0: %v", err)
	}
	if err := gw.Serve(context.Background(), failoverPod1, "10.244.1.10"); err != nil {
		t.Fatalf("Serve ordinal 1: %v", err)
	}

	var slices discoveryv1.EndpointSliceList
	if err := w.cl.List(context.Background(), &slices); err != nil {
		t.Fatal(err)
	}
	if len(slices.Items) != 1 || slices.Items[0].Endpoints[0].Addresses[0] != "10.244.1.10" {
		t.Fatalf("slices = %+v, want the one slice rewritten", slices.Items)
	}
	var svcs corev1.ServiceList
	if err := w.cl.List(context.Background(), &svcs); err != nil {
		t.Fatal(err)
	}
	if len(svcs.Items) != 1 || svcs.Items[0].Name != failoverSTS+"-nfs" {
		t.Errorf("services = %+v, want one named from the StatefulSet", svcs.Items)
	}
	if len(svcs.Items) == 1 && (len(svcs.Items[0].OwnerReferences) != 1 || svcs.Items[0].OwnerReferences[0].Kind != "StatefulSet") {
		t.Errorf("Service owners = %+v, want the StatefulSet", svcs.Items[0].OwnerReferences)
	}
}

func gatewayWorld(t *testing.T) (*failoverWorld, *mdsGateway, string) {
	t.Helper()
	w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(nil))...)
	gw := &mdsGateway{r: w.r.Exports}
	if err := gw.Serve(context.Background(), failoverPod0, "10.244.0.10"); err != nil {
		t.Fatal(err)
	}
	var svc corev1.Service
	key := client.ObjectKey{Namespace: testOperatorNS, Name: failoverSTS + "-nfs"}
	if err := w.cl.Get(context.Background(), key, &svc); err != nil {
		t.Fatal(err)
	}
	svc.Spec.ClusterIP = testServiceIP
	if err := w.cl.Update(context.Background(), &svc); err != nil {
		t.Fatal(err)
	}
	return w, gw, testServiceIP
}

func sliceAddresses(t *testing.T, cl client.Client) []string {
	t.Helper()
	var eps discoveryv1.EndpointSlice
	key := client.ObjectKey{Namespace: testOperatorNS, Name: utils.MDSEndpointSliceName(failoverSTS + "-nfs")}
	if err := cl.Get(context.Background(), key, &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	var out []string
	for _, e := range eps.Endpoints {
		out = append(out, e.Addresses...)
	}
	return out
}

func TestTheGatewayWithdrawsOnceAndForgetsTheFlowsToTheOldAddress(t *testing.T) {
	w, gw, svc := gatewayWorld(t)

	for range 2 {
		if err := gw.Withdraw(context.Background(), failoverPod1); err != nil {
			t.Fatalf("Withdraw: %v", err)
		}
	}

	if got := sliceAddresses(t, w.cl); len(got) != 0 {
		t.Errorf("endpoints = %v, want none after a withdrawal", got)
	}
	if want := []string{svc + ">10.244.0.10"}; !slices.Equal(w.flows.forgotten, want) {
		t.Errorf("forgotten = %v, want %v once: a second withdrawal of an empty slice asks for nothing", w.flows.forgotten, want)
	}
}

func TestTheGatewayServesTheNewAddressAndForgetsTheWithdrawnOneAgain(t *testing.T) {
	w, gw, svc := gatewayWorld(t)
	_ = gw.Withdraw(context.Background(), failoverPod1)

	for range 2 {
		if err := gw.Serve(context.Background(), failoverPod1, "10.244.1.10"); err != nil {
			t.Fatalf("Serve: %v", err)
		}
	}

	if got := sliceAddresses(t, w.cl); !slices.Equal(got, []string{"10.244.1.10"}) {
		t.Errorf("endpoints = %v, want the new address", got)
	}
	old := svc + ">10.244.0.10"
	if want := []string{old, old}; !slices.Equal(w.flows.forgotten, want) {
		t.Errorf("forgotten = %v, want the withdrawal's and the repoint's, each once", w.flows.forgotten)
	}
}

func TestTheGatewayCancelsTheForgetWhenTheSameAddressReturns(t *testing.T) {
	w, gw, svc := gatewayWorld(t)
	_ = gw.Withdraw(context.Background(), failoverPod0)

	if err := gw.Serve(context.Background(), failoverPod0, "10.244.0.10"); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	if want := []string{svc + ">10.244.0.10"}; !slices.Equal(w.flows.canceled, want) || len(w.flows.forgotten) != 1 {
		t.Errorf("canceled = %v forgotten = %v, want the pending forget canceled, not repeated", w.flows.canceled, w.flows.forgotten)
	}
}

func TestTheExportsAdapterListsTheClustersLiveExportsOnly(t *testing.T) {
	ready := boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseReady)
	degraded := boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseDegraded)
	degraded.Name = "degraded"
	otherCluster := boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseReady)
	otherCluster.Name = "other"
	otherCluster.Spec.VolumeRef = "7d1e0c55-3a2b-4f6e-9c8d-1b2a3c4d5e6f:pool-a:3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
	pending := testExport(nil)
	pending.Name = "pending"
	deleting := boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseReady)
	deleting.Name = "deleting"
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(nil), ready, degraded, otherCluster, pending, deleting)...)
	a := &mdsExports{r: w.r.Exports, clusterID: testExportClusterID}

	got, err := a.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Key.Name)
	}
	slices.Sort(names)
	if want := []string{"pending", testExportName}; !slices.Equal(names, want) {
		t.Errorf("exports = %v, want %v: not another cluster's, not a degraded one, not one being deleted", names, want)
	}
}

func TestTheExportsAdapterBuildsSpecsForBoundExportsOnly(t *testing.T) {
	bound := boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseReady)
	pending := testExport(nil)
	pending.Name = "pending"
	w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(nil), bound, pending)...)
	a := &mdsExports{r: w.r.Exports, clusterID: testExportClusterID}

	specs, err := a.Specs(context.Background(), "nqn-host")
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if len(specs) != 1 || specs[0].HostNQN != "nqn-host" || specs[0].Path != bound.Spec.ExportPath {
		t.Errorf("specs = %+v, want the one bound export with the host NQN", specs)
	}
}

func TestTheExportsAdapterRebindsOnlyTheMDSPodName(t *testing.T) {
	e := boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseReady)
	e.Status.AssembledBy = "uid-0/old"
	w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(nil), e)...)
	a := &mdsExports{r: w.r.Exports, clusterID: testExportClusterID}

	if err := a.Rebind(context.Background(), client.ObjectKeyFromObject(e), failoverPod1); err != nil {
		t.Fatalf("Rebind: %v", err)
	}

	got := loadExport(t, w.cl)
	if got.Status.MDSPodName != failoverPod1 || got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseReady ||
		got.Status.AssembledBy != "uid-0/old" {
		t.Errorf("status = %+v, want only mdsPodName rewritten: the resync decides the rest", got.Status)
	}
}

func TestO30TheSinkEmitsOnTheDriverAndOnEveryExportOfTheCluster(t *testing.T) {
	mine := boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseReady)
	other := boundExportOn(failoverPod0, simplyblockv1alpha2.NFSExportPhaseReady)
	other.Name = "other"
	other.Spec.VolumeRef = "7d1e0c55-3a2b-4f6e-9c8d-1b2a3c4d5e6f:pool-a:3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13"
	w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(nil), mine, other)...)
	sink := &mdsEvents{r: w.r, namespace: testOperatorNS, driver: "simplyblock", exports: &mdsExports{r: w.r.Exports, clusterID: testExportClusterID}}

	sink.Driver(corev1.EventTypeNormal, "MDSFenced", "fenced")
	sink.Exports(corev1.EventTypeWarning, "MDSFenceFailed", "failed")

	if got := w.events.about("MDSFenced"); len(got) != 1 || got[0].name != "simplyblock" {
		t.Errorf("MDSFenced = %+v, want one on the driver", got)
	}
	got := w.events.about("MDSFenceFailed")
	if len(got) != 1 || got[0].name != testExportName || got[0].eventType != corev1.EventTypeWarning {
		t.Errorf("MDSFenceFailed = %+v, want one on this cluster's export and none on the other's", got)
	}
}

func wantNQN(t *testing.T, ordinal int32) string {
	t.Helper()
	n, err := mdsstate.HostNQN(testMDSStatefulSetUID, ordinal)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The export calls carry the host NQN of the pod's own ordinal, which is what
// the promotion registered and what the control plane authorized.
func TestExportCallsCarryTheNQNOfThePodsOrdinal(t *testing.T) {
	asm := &fakeAssembler{}
	pod := mdsPod()
	pod.Name = failoverPod1
	e := testExport(bound(simplyblockv1alpha2.NFSExportPhaseAssembling))
	e.Status.MDSPodName = failoverPod1
	r, _ := newExportReconciler(t, asm, e, pod)

	reconcileExport(t, r)

	if want := wantNQN(t, 1); len(asm.hostNQNs) != 1 || asm.hostNQNs[0] != want {
		t.Errorf("host NQNs = %v, want [%s], the ordinal 1 identity", asm.hostNQNs, want)
	}
}

func TestTheFailoverAndDriverAgreeOnTheNamesTheyShare(t *testing.T) {
	if mdsfailover.ClusterLabel != driver.MDSClusterLabel || mdsfailover.ContainerName != driver.MDSContainerName {
		t.Errorf("label %q/%q or container %q/%q differ", mdsfailover.ClusterLabel, driver.MDSClusterLabel,
			mdsfailover.ContainerName, driver.MDSContainerName)
	}
}

func TestO16AnExportWaitsPendingWhileTheRecordedTargetIsNotActive(t *testing.T) {
	guests := newFakeGuests()
	sts := failoverStatefulSet(map[string]string{
		mdsfailover.AnnotationActive: failoverPod1, mdsfailover.AnnotationActiveInstance: "uid-1/containerd://uid-1",
		mdsfailover.AnnotationEpoch: "2",
	})
	objects := failoverObjects(sts, testExport(nil))
	r, cl, events := newPodHostedReconciler(t, objects...)
	r.Serving = &mdsServingGate{Client: cl, Namespace: testOperatorNS, Guests: guests}

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending || got.Status.MDSPodName != "" {
		t.Errorf("phase = %q, mdsPodName = %q, want Pending and unbound while the target is a standby",
			got.Status.Phase, got.Status.MDSPodName)
	}
	if !slices.Contains(events.reasons, "WaitingForActiveMDS") {
		t.Errorf("events = %v, want WaitingForActiveMDS", events.reasons)
	}

	guests.roles[failoverPod1] = nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: 2}
	reconcileExport(t, r)
	got = loadExport(t, cl)
	if got.Status.MDSPodName != failoverPod1 || got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Errorf("phase = %q, mdsPodName = %q, want bound to the active %s once it serves", got.Status.Phase,
			got.Status.MDSPodName, failoverPod1)
	}
}

func TestTheGateNamesWhyNoPodServesYet(t *testing.T) {
	guests := newFakeGuests()
	for name, c := range map[string]struct {
		annotations map[string]string
		describe    nfsexportrpc.Description
		session     bool
		wantPod     string
	}{
		"no promotion yet": {nil, nfsexportrpc.Description{}, true, ""},
		"target standby": {map[string]string{
			mdsfailover.AnnotationActive: failoverPod1, mdsfailover.AnnotationEpoch: "2",
		}, nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby}, true, ""},
		"target promoting": {map[string]string{
			mdsfailover.AnnotationActive: failoverPod1, mdsfailover.AnnotationEpoch: "2",
		}, nfsexportrpc.Description{Role: nfsexportrpc.RolePromoting}, true, ""},
		"active at an older epoch": {map[string]string{
			mdsfailover.AnnotationActive: failoverPod1, mdsfailover.AnnotationEpoch: "2",
		}, nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: 1}, true, ""},
		"no session": {map[string]string{
			mdsfailover.AnnotationActive: failoverPod1, mdsfailover.AnnotationEpoch: "2",
		}, nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: 2}, false, ""},
		"serving": {map[string]string{
			mdsfailover.AnnotationActive: failoverPod1, mdsfailover.AnnotationEpoch: "2",
		}, nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: 2}, true, failoverPod1},
	} {
		t.Run(name, func(t *testing.T) {
			w := newFailoverWorld(t, failoverObjects(failoverStatefulSet(c.annotations))...)
			guests.roles[failoverPod1] = c.describe
			guests.sessions[failoverPod1] = c.session
			gate := &mdsServingGate{Client: w.cl, Namespace: testOperatorNS, Guests: guests}

			pod, why, err := gate.ServingPod(context.Background(), failoverSTS)
			if err != nil || pod != c.wantPod {
				t.Fatalf("ServingPod = %q, %q, %v, want pod %q", pod, why, err, c.wantPod)
			}
			if pod == "" && why == "" {
				t.Error("no reason given for the wait")
			}
		})
	}
}

func TestTheGateTreatsAMissingStatefulSetAsNothingServing(t *testing.T) {
	w := newFailoverWorld(t)
	gate := &mdsServingGate{Client: w.cl, Namespace: testOperatorNS, Guests: newFakeGuests()}
	pod, _, err := gate.ServingPod(context.Background(), failoverSTS)
	if err != nil || pod != "" {
		t.Errorf("ServingPod = %q, %v, want nothing and no error", pod, err)
	}
	_ = apierrors.IsNotFound
	_ = strconv.Itoa
}
