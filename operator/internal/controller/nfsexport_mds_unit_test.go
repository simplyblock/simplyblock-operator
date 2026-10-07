package controller

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// reasonRecorder keeps the reason of every event, so a test can assert that
// a refusal to act said why.
type reasonRecorder struct{ reasons []string }

func (r *reasonRecorder) Eventf(_ runtime.Object, _ runtime.Object, _, reason, _, _ string, _ ...interface{}) {
	r.reasons = append(r.reasons, reason)
}

// The storage cluster in testExport's volumeRef.
const testExportClusterID = "0f2ac1d3-9b7e-4c21-8a55-6d4e3f1b2c90"

func mdsDriver() *simplyblockv1alpha2.SimplyblockDriver {
	return &simplyblockv1alpha2.SimplyblockDriver{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock", Namespace: testOperatorNS, UID: "driver-uid"},
		Spec: simplyblockv1alpha2.SimplyblockDriverSpec{
			Image: "quay.io/simplyblock-io/spdkcsi:v26.3.0",
			PNFS: simplyblockv1alpha2.DriverPNFS{MDS: &simplyblockv1alpha2.DriverPNFSMDS{
				Image:     "quay.io/simplyblock-io/spdkcsi:pnfs-mds-v26.3.0",
				StateSize: resource.MustParse("1Gi"),
			}},
		},
	}
}

func boundMDSPod(mutate func(*corev1.Pod)) *corev1.Pod {
	d := mdsDriver()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: driver.MDSPodName(d, testExportClusterID), Namespace: testOperatorNS, UID: "mds-uid-1",
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
// KVM-capable node, or a state disk that does not bind.
func TestPendingPodHostedNamesWhyThePodCannotSchedule(t *testing.T) {
	cases := map[string]struct {
		message, reason string
	}{
		"no KVM node": {
			"0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.",
			"NoKVMCapableNode",
		},
		"state disk": {
			"0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims.",
			"MDSStateUnavailable",
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
// export's Service endpoints at. No node is named: the pod is the host.
func TestPendingPodHostedBindsTheReadyPod(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), boundMDSPod(readyPod))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase != simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Fatalf("phase = %q, want Assembling", got.Status.Phase)
	}
	if want := driver.MDSPodName(mdsDriver(), testExportClusterID); got.Status.MDSPodName != want {
		t.Errorf("mdsPodName = %q, want %q", got.Status.MDSPodName, want)
	}
	if got.Status.MDSNodeIP != testMDSPodIP || got.Status.MDSNodeName != "" {
		t.Errorf("mdsNodeIP = %q, mdsNodeName = %q, want the pod IP and no node",
			got.Status.MDSNodeIP, got.Status.MDSNodeName)
	}
	if len(got.Status.AllowedClients) == 0 {
		t.Error("bound with no client set, which the guest would refuse")
	}
	var eps discoveryv1.EndpointSlice
	epsKey := client.ObjectKey{Name: utils.NFSExportEndpointSliceName(testExportName), Namespace: testExportNS}
	if err := cl.Get(context.Background(), epsKey, &eps); err != nil {
		t.Fatalf("reading the EndpointSlice: %v", err)
	}
	if len(eps.Endpoints) != 1 || len(eps.Endpoints[0].Addresses) != 1 ||
		eps.Endpoints[0].Addresses[0] != testMDSPodIP {
		t.Errorf("EndpointSlice endpoints = %+v, want only the pod IP", eps.Endpoints)
	}
}

// Without spec.pnfs.mds the metadata server stays on nodes, and no
// StatefulSet appears.
func TestPendingWithoutTheMDSSpecStaysNodeHosted(t *testing.T) {
	d := mdsDriver()
	d.Spec.PNFS.MDS = nil
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), d, testNode(testMDSHost, nil))

	reconcileExport(t, r)

	var list appsv1.StatefulSetList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("created %d StatefulSets for a node-hosted deployment", len(list.Items))
	}
	if got := loadExport(t, cl); got.Status.MDSNodeName != testMDSHost {
		t.Errorf("mdsNodeName = %q, want the labeled node", got.Status.MDSNodeName)
	}
}
