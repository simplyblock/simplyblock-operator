package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
)

// reasonRecorder keeps the reason of every event, so a test can assert that
// a refusal to act said why.
type reasonRecorder struct{ reasons, messages []string }

func (r *reasonRecorder) Eventf(_ runtime.Object, _ runtime.Object, _, reason, _, note string, args ...interface{}) {
	r.reasons = append(r.reasons, reason)
	r.messages = append(r.messages, fmt.Sprintf(note, args...))
}

// The storage cluster in testExport's volumeRef.
const testExportClusterID = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"

func mdsDriver() *simplyblockv1alpha2.SimplyblockDriver {
	return &simplyblockv1alpha2.SimplyblockDriver{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: testOperatorNS, UID: "driver-uid"},
		Spec: simplyblockv1alpha2.SimplyblockDriverSpec{
			Image: "quay.io/simplyblock-io/spdkcsi:v26.3.0",
			PNFS: simplyblockv1alpha2.DriverPNFS{MDS: &simplyblockv1alpha2.DriverPNFSMDS{
				Image: "quay.io/simplyblock-io/spdkcsi:pnfs-mds-v26.3.0",
			}},
		},
	}
}

func boundMDSPod(mutate func(*corev1.Pod)) *corev1.Pod {
	d := mdsDriver()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: driver.MDSPodName(d, testExportClusterID, 0), Namespace: testOperatorNS, UID: "mds-uid-1",
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func readyPod(p *corev1.Pod) {
	p.Status.PodIP = testMDSPodIP
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
}

func unschedulableBecause(message string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, Message: message,
		}}
	}
}

func newPodHostedReconciler(
	t *testing.T, objects ...client.Object,
) (*NFSExportReconciler, client.Client, *reasonRecorder) {
	t.Helper()
	scheme := newTestScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme, appsv1.AddToScheme)
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha2.NFSExport{}},
		withBaselineKubeNode(objects)...,
	)
	recorder := &reasonRecorder{}
	return &NFSExportReconciler{
		Client:            cl,
		Scheme:            scheme,
		Recorder:          recorder,
		Assembler:         &fakeAssembler{},
		OperatorNamespace: testOperatorNS,
	}, cl, recorder
}

// mdsTemplateHashKey is the annotation the operator records the metadata
// server's pod template under, spelled out so the tests do not depend on the
// constant they check.
const mdsTemplateHashKey = "storage.simplyblock.io/mds-template-hash"

// staleMDSStatefulSet is the StatefulSet an earlier operator release created:
// another runner image and a state disk class the current lookup would not
// pick. With keepHash it carries that release's own template hash, as one
// written by a hash-aware release does. Without it, it has none, as one
// written before the hash existed.
func staleMDSStatefulSet(t *testing.T, keepHash bool) *appsv1.StatefulSet {
	t.Helper()
	old := mdsDriver()
	old.Spec.PNFS.MDS.Image = "quay.io/simplyblock-io/spdkcsi:pnfs-mds-v26.2.0"
	_, sts, err := driver.MDSObjects(old, testExportClusterID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !keepHash {
		delete(sts.Annotations, mdsTemplateHashKey)
	}
	return sts
}

// currentMDSHash is the template hash the current driver's StatefulSet carries.
func currentMDSHash(t *testing.T) string {
	t.Helper()
	_, sts, err := driver.MDSObjects(mdsDriver(), testExportClusterID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sts.Annotations[mdsTemplateHashKey]
}

func loadMDSStatefulSet(t *testing.T, cl client.Client) *appsv1.StatefulSet {
	t.Helper()
	var sts appsv1.StatefulSet
	key := client.ObjectKey{Namespace: testOperatorNS, Name: driver.MDSStatefulSetName(mdsDriver(), testExportClusterID)}
	if err := cl.Get(context.Background(), key, &sts); err != nil {
		t.Fatalf("reading the metadata server StatefulSet: %v", err)
	}
	return &sts
}

// Regression: 2026-10-09-mds-statefulset-never-updated. An operator upgrade
// moved the CSI plugins to a new image and left the metadata server on the old
// one, because an existing StatefulSet was never written again. The pod
// template follows the driver. The claim templates, which Kubernetes does not
// let change, stay as they were.
func TestPendingPodHostedUpdatesAnOutdatedMDSStatefulSet(t *testing.T) {
	// The previous release's own hash, not none: a comparison that only
	// noticed a missing hash would leave this one alone.
	r, cl, events := newPodHostedReconciler(t, testExport(nil), mdsDriver(), staleMDSStatefulSet(t, true))

	reconcileExport(t, r)

	sts := loadMDSStatefulSet(t, cl)
	if got, want := sts.Spec.Template.Spec.Containers[0].Image, mdsDriver().Spec.PNFS.MDS.Image; got != want {
		t.Errorf("runner image = %q, want %q", got, want)
	}
	if got, want := sts.Annotations[mdsTemplateHashKey], currentMDSHash(t); got != want {
		t.Errorf("template hash = %q, want the current %q", got, want)
	}
	if !slices.Contains(events.reasons, "MDSUpdated") {
		t.Errorf("events = %v, want MDSUpdated", events.reasons)
	}
}

// A StatefulSet already matching the driver is not written, so a reconcile
// restarts no guest.
func TestPendingPodHostedLeavesAnUpToDateMDSStatefulSet(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver())
	reconcileExport(t, r)
	before := loadMDSStatefulSet(t, cl).ResourceVersion

	reconcileExport(t, r)

	if after := loadMDSStatefulSet(t, cl).ResourceVersion; after != before {
		t.Errorf("resourceVersion %s -> %s: an unchanged StatefulSet was written", before, after)
	}
}

// A new StatefulSet carries the hash of its template, so the next release can
// tell whether it is current.
func TestPendingPodHostedCreatesTheMDSStatefulSetWithItsTemplateHash(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver())

	reconcileExport(t, r)

	if sts := loadMDSStatefulSet(t, cl); sts.Annotations[mdsTemplateHashKey] == "" {
		t.Errorf("annotations = %v, want the template hash", sts.Annotations)
	}
}

// readyMDSExport is an export bound to its cluster's metadata server, the
// state every export is in on a stable cluster when the operator is upgraded.
func readyMDSExport() *simplyblockv1alpha2.NFSExport {
	return testExport(bound(simplyblockv1alpha2.NFSExportPhaseReady))
}

// Regression: 2026-10-09-mds-statefulset-never-updated, the Ready half. Only a
// Pending export ever looked at the StatefulSet, and after an upgrade on a
// stable cluster every export is Ready, so the template stayed old for good.
// A Ready export updates it and stays Ready: the guest restart that follows is
// what the restart resync handles.
func TestReadyExportUpdatesAnOutdatedMDSStatefulSet(t *testing.T) {
	// No hash at all: the StatefulSet a release before the hash wrote.
	r, cl, events := newPodHostedReconciler(t, readyMDSExport(), mdsDriver(), mdsPod(), staleMDSStatefulSet(t, false))

	reconcileExport(t, r)

	sts := loadMDSStatefulSet(t, cl)
	if got, want := sts.Spec.Template.Spec.Containers[0].Image, mdsDriver().Spec.PNFS.MDS.Image; got != want {
		t.Errorf("runner image = %q, want %q", got, want)
	}
	if got, want := sts.Annotations[mdsTemplateHashKey], currentMDSHash(t); got != want {
		t.Errorf("template hash = %q, want the current %q", got, want)
	}
	if !slices.Contains(events.reasons, "MDSUpdated") {
		t.Errorf("events = %v, want MDSUpdated", events.reasons)
	}
	if got := loadExport(t, cl).Status.Phase; got != simplyblockv1alpha2.NFSExportPhaseReady {
		t.Errorf("phase = %q, want it left Ready", got)
	}
}

// A Ready reconcile of a current StatefulSet writes nothing, and a Ready export
// whose StatefulSet is gone does not create one: creating is the Pending
// path's, which also picks the state disk class.
func TestReadyExportLeavesTheMDSStatefulSetAsItIs(t *testing.T) {
	t.Run("up to date", func(t *testing.T) {
		_, current, err := driver.MDSObjects(mdsDriver(), testExportClusterID, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, cl, events := newPodHostedReconciler(t, readyMDSExport(), mdsDriver(), mdsPod(), current)
		before := loadMDSStatefulSet(t, cl).ResourceVersion

		reconcileExport(t, r)

		if after := loadMDSStatefulSet(t, cl).ResourceVersion; after != before {
			t.Errorf("resourceVersion %s -> %s: an unchanged StatefulSet was written", before, after)
		}
		if slices.Contains(events.reasons, "MDSUpdated") {
			t.Errorf("events = %v, want no MDSUpdated", events.reasons)
		}
	})
	t.Run("absent", func(t *testing.T) {
		r, cl, _ := newPodHostedReconciler(t, readyMDSExport(), mdsDriver(), mdsPod())

		reconcileExport(t, r)

		var sts appsv1.StatefulSet
		key := client.ObjectKey{Namespace: testOperatorNS, Name: driver.MDSStatefulSetName(mdsDriver(), testExportClusterID)}
		if err := cl.Get(context.Background(), key, &sts); !apierrors.IsNotFound(err) {
			t.Errorf("Get = %v, want NotFound: a Ready export created the StatefulSet", err)
		}
	})
}

// conflictingUpdates fails the first StatefulSet update with a conflict after
// writing current in its place, which is another export of the same cluster
// winning the race.
type conflictingUpdates struct {
	client.Client
	current *appsv1.StatefulSet
	fired   bool
}

func (c *conflictingUpdates) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if sts, ok := obj.(*appsv1.StatefulSet); ok && !c.fired {
		c.fired = true
		var live appsv1.StatefulSet
		if err := c.Get(ctx, client.ObjectKeyFromObject(sts), &live); err != nil {
			return err
		}
		live.Spec.Template = c.current.Spec.Template
		live.Annotations = c.current.Annotations
		if err := c.Client.Update(ctx, &live); err != nil {
			return err
		}
		return apierrors.NewConflict(appsv1.Resource("statefulsets"), sts.Name, errors.New("modified"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

// Two exports of one cluster can both find the template old. The one that
// loses the update finds the current hash on a re-read and is done, rather
// than failing its reconcile.
func TestAnMDSUpdateThatLostTheRaceToTheCurrentTemplateSucceeds(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, readyMDSExport(), mdsDriver(), mdsPod(), staleMDSStatefulSet(t, false))
	_, current, err := driver.MDSObjects(mdsDriver(), testExportClusterID, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Client = &conflictingUpdates{Client: cl, current: current}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{
		Namespace: testExportNS, Name: testExportName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := loadMDSStatefulSet(t, cl).Annotations[mdsTemplateHashKey]; got != current.Annotations[mdsTemplateHashKey] {
		t.Errorf("hash = %q, want the current one", got)
	}
}

// The first export of a storage cluster brings its metadata server up: the
// StatefulSet and its ServiceAccount, owned by the driver so that turning
// pNFS off removes them.
func TestPendingPodHostedCreatesTheMDSStatefulSet(t *testing.T) {
	d := mdsDriver()
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), d)

	res := reconcileExport(t, r)

	var sts appsv1.StatefulSet
	key := client.ObjectKey{Namespace: testOperatorNS, Name: driver.MDSStatefulSetName(d, testExportClusterID)}
	if err := cl.Get(context.Background(), key, &sts); err != nil {
		t.Fatalf("the metadata server StatefulSet was not created: %v", err)
	}
	if len(sts.OwnerReferences) != 1 || sts.OwnerReferences[0].UID != d.UID {
		t.Errorf("StatefulSet owners = %+v, want the driver", sts.OwnerReferences)
	}
	var sa corev1.ServiceAccount
	if err := cl.Get(context.Background(),
		client.ObjectKey{Namespace: testOperatorNS, Name: sts.Spec.Template.Spec.ServiceAccountName}, &sa); err != nil {
		t.Errorf("the metadata server ServiceAccount was not created: %v", err)
	}
	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending || got.Status.MDSPodName != "" {
		t.Errorf("phase = %q, mdsPodName = %q, want Pending and unbound until the pod is Ready",
			got.Status.Phase, got.Status.MDSPodName)
	}
	if res.RequeueAfter == 0 {
		t.Error("no requeue while waiting for the metadata server pod")
	}
}

// A pod that cannot schedule leaves the export waiting, and says why: no
// KVM-capable node.
func TestPendingPodHostedNamesWhyThePodCannotSchedule(t *testing.T) {
	cases := map[string]struct {
		message, reason string
	}{
		"no KVM node": {
			"0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.",
			"NoKVMCapableNode",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, cl, events := newPodHostedReconciler(t, testExport(nil), mdsDriver(),
				boundMDSPod(unschedulableBecause(c.message)))

			reconcileExport(t, r)

			if !slices.Contains(events.reasons, c.reason) {
				t.Errorf("events = %v, want %s", events.reasons, c.reason)
			}
			if got := loadExport(t, cl); got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending {
				t.Errorf("phase = %q, want Pending", got.Status.Phase)
			}
		})
	}
}

// A scheduled pod whose guest has not turned healthy is not bound to: an
// export assembled against it would wait out its deadline on a guest that may
// never answer.
func TestPendingPodHostedWaitsForTheGuest(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), boundMDSPod(func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
	}))

	res := reconcileExport(t, r)

	if got := loadExport(t, cl); got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending ||
		got.Status.MDSPodName != "" {
		t.Errorf("phase = %q, mdsPodName = %q, want Pending and unbound", got.Status.Phase, got.Status.MDSPodName)
	}
	if res.RequeueAfter == 0 {
		t.Error("no requeue while the guest boots")
	}
}

// A Ready pod is bound by name and addressed by its IP, which is what the
// export's Service endpoints at.
func TestPendingPodHostedBindsTheReadyPod(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), boundMDSPod(readyPod))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Fatalf("phase = %q, want Assembling", got.Status.Phase)
	}
	if want := driver.MDSPodName(mdsDriver(), testExportClusterID, 0); got.Status.MDSPodName != want {
		t.Errorf("mdsPodName = %q, want %q", got.Status.MDSPodName, want)
	}
	if got.Status.MDSNodeIP != testMDSPodIP {
		t.Errorf("mdsNodeIP = %q, want the pod IP", got.Status.MDSNodeIP)
	}
	if len(got.Status.AllowedClients) == 0 {
		t.Error("bound with no client set, which the guest would refuse")
	}
	var eps discoveryv1.EndpointSlice
	epsKey := mdsSliceKeyFor(driver.MDSPodName(mdsDriver(), testExportClusterID, 0))
	if err := cl.Get(context.Background(), epsKey, &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	if len(eps.Endpoints) != 1 || len(eps.Endpoints[0].Addresses) != 1 ||
		eps.Endpoints[0].Addresses[0] != testMDSPodIP {
		t.Errorf("EndpointSlice endpoints = %+v, want only the pod IP", eps.Endpoints)
	}
}

// Without spec.pnfs.mds there is no metadata server to bind, and an export
// waits rather than falling back to a node's own nfsd, which could never fail
// over. The status says what is missing, so the wait is not mistaken for a
// reconcile that never ran.
func TestPendingWithoutTheMDSSpecWaitsForIt(t *testing.T) {
	d := mdsDriver()
	d.Spec.PNFS.MDS = nil
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), d, kubeNode("worker-1", testNodeIP))

	res := reconcileExport(t, r)

	var list appsv1.StatefulSetList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("created %d StatefulSets with no metadata server configured", len(list.Items))
	}
	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending || got.Status.MDSPodName != "" {
		t.Errorf("phase = %q, mdsPodName = %q, want Pending and unbound", got.Status.Phase, got.Status.MDSPodName)
	}
	if !strings.Contains(got.Status.Message, "spec.pnfs.mds") {
		t.Errorf("message = %q, want it to name spec.pnfs.mds", got.Status.Message)
	}
	if res.RequeueAfter != nfsExportNoHostRequeue {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, nfsExportNoHostRequeue)
	}
}

// nodeWithPodCIDR is a node whose pod network is 10.244.2.0/24.
func nodeWithPodCIDR() *corev1.Node {
	n := kubeNode("worker-3", "192.168.10.144")
	n.Spec.PodCIDR = "10.244.2.0/24"
	n.Spec.PodCIDRs = []string{"10.244.2.0/24"}
	return n
}

// A node reaches the metadata server pod across the pod network, and the CNI
// rewrites its source to the node's address in its pod CIDR, which is what the
// guest's nfsd sees. Only the InternalIP in the client set refuses every mount.
func TestPendingPodHostedAllowsTheNodesPodCIDRs(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), boundMDSPod(readyPod), nodeWithPodCIDR())

	reconcileExport(t, r)

	got := loadExport(t, cl).Status.AllowedClients
	for _, want := range []string{"192.168.10.144", "10.244.2.0/24"} {
		if !slices.Contains(got, want) {
			t.Errorf("allowedClients = %v, want %s in it", got, want)
		}
	}
}

// A StatefulSet already carrying this cluster's metadata server name but
// serving another storage cluster is not this cluster's metadata server. The
// export waits and says why, rather than binding to the other cluster's pod.
func TestPendingPodHostedRefusesAnotherClustersStatefulSet(t *testing.T) {
	d := mdsDriver()
	other := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: driver.MDSStatefulSetName(d, testExportClusterID), Namespace: testOperatorNS,
		Labels: map[string]string{driver.MDSClusterLabel: "7d1e0c55-3a2b-4f6e-9c8d-1b2a3c4d5e6f"},
	}}
	r, cl, recorder := newPodHostedReconciler(t, testExport(nil), d, other)

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Name: testExportName, Namespace: testExportNS},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v; a name taken by another cluster is a wait, not an error to retry hot", err)
	}
	if !slices.Contains(recorder.reasons, "MDSNameCollision") {
		t.Errorf("event reasons = %v, want MDSNameCollision", recorder.reasons)
	}
	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending || got.Status.MDSPodName != "" {
		t.Errorf("phase = %q, mdsPodName = %q, want Pending and unbound", got.Status.Phase, got.Status.MDSPodName)
	}
	if res.RequeueAfter == 0 {
		t.Error("no requeue")
	}
}

// A memory limit the guest cannot boot in is refused before the StatefulSet is
// created, in case it got past admission: the webhook ignores its own
// failures. The export waits and says why.
func TestPendingPodHostedRefusesAnMDSMemoryLimitUnder512Mi(t *testing.T) {
	d := mdsDriver()
	d.Spec.PNFS.MDS.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
	r, cl, recorder := newPodHostedReconciler(t, testExport(nil), d)

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Name: testExportName, Namespace: testExportNS},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var created appsv1.StatefulSetList
	if err := cl.List(context.Background(), &created); err != nil || len(created.Items) != 0 {
		t.Fatalf("the StatefulSet was created for a guest that cannot boot: %v %v", created.Items, err)
	}
	if !slices.Contains(recorder.reasons, "MDSResourcesInvalid") {
		t.Errorf("event reasons = %v, want MDSResourcesInvalid", recorder.reasons)
	}
	if res.RequeueAfter == 0 {
		t.Error("no requeue: a corrected limit would never be noticed")
	}
}

// Limits the runner refuses stop a Ready export's update as they stop a
// create, and the guest already running keeps serving: the StatefulSet is not
// written, the export stays Ready, and the reconcile does not fail.
func TestReadyExportWithInvalidLimitsLeavesTheMDSStatefulSet(t *testing.T) {
	d := mdsDriver()
	d.Spec.PNFS.MDS.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
	r, cl, events := newPodHostedReconciler(t, readyMDSExport(), d, mdsPod(), staleMDSStatefulSet(t, true))
	before := loadMDSStatefulSet(t, cl)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{
		Namespace: testExportNS, Name: testExportName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after := loadMDSStatefulSet(t, cl)
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion %s -> %s: the StatefulSet was written", before.ResourceVersion, after.ResourceVersion)
	}
	if !slices.Contains(events.reasons, "MDSResourcesInvalid") {
		t.Errorf("events = %v, want MDSResourcesInvalid", events.reasons)
	}
	if got := loadExport(t, cl).Status.Phase; got != simplyblockv1alpha2.NFSExportPhaseReady {
		t.Errorf("phase = %q, want it left Ready", got)
	}
}

// failingUpdates fails every StatefulSet update, as an API server outage does.
type failingUpdates struct{ client.Client }

func (c *failingUpdates) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*appsv1.StatefulSet); ok {
		return errors.New("the server is currently unable to handle the request")
	}
	return c.Client.Update(ctx, obj, opts...)
}

// A Pending export whose metadata server could not be updated says so rather
// than that it could not be created: the StatefulSet exists, and the event is
// what an operator reads first (review on #707).
func TestPendingEventsDoNotCallAFailedUpdateACreate(t *testing.T) {
	r, cl, events := newPodHostedReconciler(t, testExport(nil), mdsDriver(), staleMDSStatefulSet(t, true))
	r.Client = &failingUpdates{Client: cl}

	_, _ = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{
		Namespace: testExportNS, Name: testExportName}})

	if len(events.messages) == 0 {
		t.Fatal("no event for the failed update")
	}
	for _, m := range events.messages {
		if strings.Contains(m, "cannot create") {
			t.Errorf("event %q calls the failed update a create", m)
		}
	}
}
