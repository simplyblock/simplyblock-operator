package controller

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	"github.com/simplyblock/simplyblock-operator/internal/utils"

	"github.com/simplyblock/atlas/kube"
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
	scheme := newTestScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme, appsv1.AddToScheme,
		storagev1.AddToScheme)
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha2.NFSExport{}},
		withBaselineStateClass(withBaselineKubeNode(objects))...,
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

// testStateClass is the simplyblock class of testExport's storage cluster.
const testStateClass = "simplyblock-cluster-a"

// storageClass is a StorageClass of provisioner for clusterID, formatting with
// fsType.
func storageClass(name, provisioner, clusterID, fsType string) *storagev1.StorageClass {
	params := map[string]string{}
	if clusterID != "" {
		params[kube.ParamClusterID] = clusterID
	}
	if fsType != "" {
		params[kube.ParamFSType] = fsType
	}
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
		Parameters:  params,
	}
}

// withBaselineStateClass gives a test that names no StorageClass the one its
// metadata server's state disk needs, so only the tests about that choice
// have to think about it.
func withBaselineStateClass(objects []client.Object) []client.Object {
	for _, o := range objects {
		if _, ok := o.(*storagev1.StorageClass); ok {
			return objects
		}
	}
	return append(objects,
		storageClass(testStateClass, driver.DefaultDriverName, testExportClusterID, "xfs"))
}

// stateClassOf is the class the StatefulSet's state disk claims from, or ""
// when the StatefulSet was not created.
func stateClassOf(t *testing.T, cl client.Client, d *simplyblockv1alpha2.SimplyblockDriver) string {
	t.Helper()
	var sts appsv1.StatefulSet
	key := client.ObjectKey{Namespace: testOperatorNS, Name: driver.MDSStatefulSetName(d, testExportClusterID)}
	if err := cl.Get(context.Background(), key, &sts); err != nil {
		return ""
	}
	if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName == nil {
		t.Fatalf("the state disk claim names no class: %+v", sts.Spec.VolumeClaimTemplates)
	}
	return *sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName
}

// The state disk is a simplyblock volume of the storage cluster the metadata
// server serves, so the pod restarts on any worker with its client-recovery
// database. Left to the cluster's default class it would be wherever that
// points, typically a node-local path that pins the pod to one node for good.
func TestPodHostedStateDiskDefaultsToTheClustersSimplyblockClass(t *testing.T) {
	d := mdsDriver()
	local := storageClass("local-path", "rancher.io/local-path", "", "")
	local.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), d,
		local,
		// A pNFS class provisions an export, not a block device.
		storageClass("a-pnfs", driver.DefaultDriverName, testExportClusterID, kube.FSTypePNFS),
		// Simplyblock, but another storage cluster's.
		storageClass("another-cluster", driver.DefaultDriverName, "7d1e0c55-3a2b-4f6e-9c8d-1b2a3c4d5e6f", "xfs"),
		storageClass(testStateClass, driver.DefaultDriverName, testExportClusterID, "xfs"),
	)

	reconcileExport(t, r)

	if got := stateClassOf(t, cl, d); got != testStateClass {
		t.Fatalf("state disk class = %q, want the storage cluster's own simplyblock class %q", got, testStateClass)
	}
}

// A named class is used as named, provided simplyblock provisions it.
func TestPodHostedStateDiskTakesANamedSimplyblockClass(t *testing.T) {
	d := mdsDriver()
	d.Spec.PNFS.MDS.StateStorageClassName = ptr.To("chosen")
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), d,
		storageClass(testStateClass, driver.DefaultDriverName, testExportClusterID, "xfs"),
		storageClass("chosen", driver.DefaultDriverName, testExportClusterID, "ext4"),
	)

	reconcileExport(t, r)

	if got := stateClassOf(t, cl, d); got != "chosen" {
		t.Fatalf("state disk class = %q, want the named %q", got, "chosen")
	}
}

// Nothing other than a simplyblock volume is accepted for the state disk,
// named or not. The export waits, the StatefulSet is not created, since its
// claim template could not be corrected afterward, and an event says why.
func TestPodHostedStateDiskRefusesAClassThatIsNotSimplyblock(t *testing.T) {
	cases := map[string]struct {
		named   string
		classes []client.Object
	}{
		"a named class another provisioner serves": {
			named: "local-path",
			classes: []client.Object{
				storageClass("local-path", "rancher.io/local-path", "", ""),
				storageClass(testStateClass, driver.DefaultDriverName, testExportClusterID, "xfs"),
			},
		},
		"a named class that does not exist": {
			named:   "missing",
			classes: []client.Object{storageClass(testStateClass, driver.DefaultDriverName, testExportClusterID, "xfs")},
		},
		"a named pNFS class": {
			named:   "a-pnfs",
			classes: []client.Object{storageClass("a-pnfs", driver.DefaultDriverName, testExportClusterID, kube.FSTypePNFS)},
		},
		"no simplyblock class of this storage cluster": {
			classes: []client.Object{
				storageClass("local-path", "rancher.io/local-path", "", ""),
				storageClass("another-cluster", driver.DefaultDriverName, "7d1e0c55-3a2b-4f6e-9c8d-1b2a3c4d5e6f", "xfs"),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := mdsDriver()
			if tc.named != "" {
				d.Spec.PNFS.MDS.StateStorageClassName = ptr.To(tc.named)
			}
			r, cl, recorder := newPodHostedReconciler(t, append([]client.Object{testExport(nil), d}, tc.classes...)...)

			res, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKey{Name: testExportName, Namespace: testExportNS},
			})
			if err != nil {
				t.Fatalf("Reconcile: %v; a class that cannot be used is a wait, not an error to retry hot", err)
			}

			if got := stateClassOf(t, cl, d); got != "" {
				t.Fatalf("the StatefulSet was created with state disk class %q", got)
			}
			if !slices.Contains(recorder.reasons, "MDSStateUnavailable") {
				t.Errorf("event reasons = %v, want MDSStateUnavailable", recorder.reasons)
			}
			if got := loadExport(t, cl); got.Status.Phase != simplyblockv1alpha2.NFSExportPhasePending {
				t.Errorf("phase = %q, want Pending", got.Status.Phase)
			}
			if res.RequeueAfter == 0 {
				t.Error("no requeue: a class created later would never be noticed")
			}
		})
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

// A node-hosted metadata server sees clients from their InternalIPs, so the
// pod network stays out of its client set.
func TestNodeHostedClientSetLeavesThePodNetworkOut(t *testing.T) {
	node := nodeWithPodCIDR()
	node.Labels[MDSCapableLabel] = "true"
	r, cl := newExportReconciler(t, &fakeAssembler{}, testExport(nil), node)

	reconcileExport(t, r)

	if got := loadExport(t, cl).Status.AllowedClients; slices.Contains(got, "10.244.2.0/24") {
		t.Errorf("allowedClients = %v, want no pod CIDR", got)
	}
}
