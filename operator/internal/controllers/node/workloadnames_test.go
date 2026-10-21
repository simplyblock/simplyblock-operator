// Tests for a second cluster in a namespace (design-superclusters.md §6.1): its
// workload is built under the names its status records, it takes nothing
// another cluster holds, it publishes only its own SPDK pods, and its nodes are
// addressed through its own Service.

package node

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	secondCluster     = "second"
	secondClusterUUID = "7c41e2aa-5d0b-4c3e-9f11-2b6a8d4e0c99"
	firstClusterUUID  = "0a9b8c7d-1111-4222-8333-944455556666"
)

// aNamedCluster is a cluster in the test namespace whose status records names.
func aNamedCluster(name, uuid string, names atlaskube.WorkloadNames) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: opsNamespace, UID: types.UID("uid-" + name)},
		Status: simplyblockv1alpha2.StorageClusterStatus{
			UUID:          uuid,
			WorkloadNames: utils.WorkloadNamesStatus(names),
		},
	}
}

func aNamesReconciler(t *testing.T, objects ...client.Object) *StorageNodeWorkloadReconciler {
	t.Helper()
	scheme := testsupport.NewScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme, rbacv1.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &StorageNodeWorkloadReconciler{
		Client: c, Scheme: scheme, Namespace: opsNamespace,
		TLSEnabled: true, TLSProvider: "openshift",
		Workload: &Workload{Client: c},
	}
}

func TestASecondClustersServicesCarryItsOwnNames(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(secondCluster)
	cluster := aNamedCluster(secondCluster, secondClusterUUID, names)
	r := aNamesReconciler(t, cluster)

	if err := r.reconcileService(context.Background(), cluster); err != nil {
		t.Fatalf("reconcileService: %v", err)
	}

	for service, secret := range map[string]string{
		names.NodeAPIService:   names.NodeAPITLSSecret,
		names.SpdkProxyService: names.SpdkProxyTLSSecret,
	} {
		var got corev1.Service
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: opsNamespace, Name: service}, &got); err != nil {
			t.Errorf("Service %s was not created: %v", service, err)
			continue
		}
		if got.Annotations[utils.OpenShiftServingCertAnnotation] != secret {
			t.Errorf("Service %s asks for certificate Secret %q, want %q",
				service, got.Annotations[utils.OpenShiftServingCertAnnotation], secret)
		}
	}
	var constant corev1.Service
	key := client.ObjectKey{Namespace: opsNamespace, Name: atlaskube.ConstantNodeAPIService}
	if err := r.Get(context.Background(), key, &constant); err == nil {
		t.Error("the second cluster created the Service with the constant name")
	}
}

// The workload never takes over a Service another cluster controls: rewriting
// its owner reference would hand the first cluster's nodes to the second, and
// deleting the second would then delete the first one's Service.
func TestTheWorkloadDoesNotTakeAServiceAnotherClusterControls(t *testing.T) {
	first := aNamedCluster("first", firstClusterUUID, atlaskube.ConstantWorkloadNames())
	held := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: atlaskube.ConstantNodeAPIService, Namespace: opsNamespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: simplyblockv1alpha2.GroupVersion.String(), Kind: "StorageCluster",
			Name: first.Name, UID: first.UID, Controller: ptr.To(true),
		}},
	}}
	second := aNamedCluster(secondCluster, "", atlaskube.ConstantWorkloadNames())
	r := aNamesReconciler(t, first, second, held)

	if err := r.reconcileService(context.Background(), second); err == nil {
		t.Error("reconcileService took a Service another cluster controls without an error")
	}

	var got corev1.Service
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(held), &got); err != nil {
		t.Fatalf("read the Service back: %v", err)
	}
	if owner := metav1.GetControllerOf(&got); owner == nil || owner.UID != first.UID {
		t.Errorf("the Service is controlled by %+v, want the first cluster", owner)
	}
}

func TestTheEndpointSliceAttachesToTheClustersOwnService(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(secondCluster)
	cluster := aNamedCluster(secondCluster, secondClusterUUID, names)
	slice := utils.BuildStorageNodeEndpointSlice(cluster, map[string]string{"worker-7.example": "10.0.0.7"})

	if got := slice.Labels[discoveryv1.LabelServiceName]; got != names.NodeAPIService {
		t.Errorf("the slice attaches to Service %q, want %q", got, names.NodeAPIService)
	}
}

func TestAMembersEndpointsCarryItsNodeLabelSuffix(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames("prod")
	names.NodeLabelSuffix = "-abc"
	cluster := aNamedCluster("prod-abc", secondClusterUUID, names)
	slice := utils.BuildStorageNodeEndpointSlice(cluster, map[string]string{"worker-7.example": "10.0.0.7"})

	if len(slice.Endpoints) != 1 || slice.Endpoints[0].Hostname == nil || *slice.Endpoints[0].Hostname != "worker-7-abc" {
		t.Errorf("endpoints = %+v, want the hostname worker-7-abc", slice.Endpoints)
	}
}

// The control plane names a cluster's SPDK pods snode-spdk-pod-<port>-<first six
// of its UUID>, and a pod without that ending comes from an older template that
// predates two clusters in a namespace.
func TestAClusterPublishesOnlyItsOwnSPDKPods(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(secondCluster)
	cluster := aNamedCluster(secondCluster, secondClusterUUID, names)
	mine := anSPDKPod("snode-spdk-pod-4420-"+secondClusterUUID[:6], "worker-2", "10.0.0.2", 4420)
	theirs := anSPDKPod("snode-spdk-pod-4420-"+firstClusterUUID[:6], "worker-1", "10.0.0.1", 4420)
	older := anSPDKPod("snode-spdk-pod-4420", "worker-3", "10.0.0.3", 4420)
	for _, pod := range []*corev1.Pod{mine, theirs, older} {
		pod.Namespace = opsNamespace
	}
	r := aNamesReconciler(t, cluster, mine, theirs, older)

	if err := r.reconcileSpdkProxyEndpoints(context.Background(), cluster); err != nil {
		t.Fatalf("reconcileSpdkProxyEndpoints: %v", err)
	}

	var slices discoveryv1.EndpointSliceList
	if err := r.List(context.Background(), &slices, client.InNamespace(opsNamespace)); err != nil {
		t.Fatalf("list the slices: %v", err)
	}
	if len(slices.Items) != 1 {
		t.Fatalf("%d slices were published, want one", len(slices.Items))
	}
	slice := slices.Items[0]
	if got := slice.Labels[discoveryv1.LabelServiceName]; got != names.SpdkProxyService {
		t.Errorf("the slice attaches to Service %q, want %q", got, names.SpdkProxyService)
	}
	if slice.Name == "spdk-proxy-endpoints-4420" {
		t.Errorf("the slice takes the name a namespace's first cluster publishes under")
	}
	published := map[string]bool{}
	for _, endpoint := range slice.Endpoints {
		published[endpoint.Addresses[0]] = true
	}
	if !published["10.0.0.2"] || !published["10.0.0.3"] || published["10.0.0.1"] {
		t.Errorf("published %v, want this cluster's pod and the older one and not the other cluster's", published)
	}
}

// The storage-node pods run as one ServiceAccount whose name the control plane's
// pod template states, so every cluster in a namespace shares it. Each owns it
// without controlling it, so it goes when the last of them does.
func TestClustersInANamespaceShareTheStorageNodeServiceAccount(t *testing.T) {
	first := aNamedCluster("first", firstClusterUUID, atlaskube.ConstantWorkloadNames())
	second := aNamedCluster(secondCluster, secondClusterUUID, atlaskube.DerivedWorkloadNames(secondCluster))
	r := aNamesReconciler(t, first, second)

	for _, cluster := range []*simplyblockv1alpha2.StorageCluster{first, second} {
		if err := r.reconcileRBAC(context.Background(), cluster); err != nil {
			t.Fatalf("reconcileRBAC(%s): %v", cluster.Name, err)
		}
	}

	var account corev1.ServiceAccount
	key := client.ObjectKey{Namespace: opsNamespace, Name: "simplyblock-storage-node-sa"}
	if err := r.Get(context.Background(), key, &account); err != nil {
		t.Fatalf("read the ServiceAccount: %v", err)
	}
	owners := map[types.UID]bool{}
	for _, owner := range account.OwnerReferences {
		owners[owner.UID] = true
		if owner.Controller != nil && *owner.Controller {
			t.Errorf("cluster %s controls the shared ServiceAccount", owner.Name)
		}
	}
	if !owners[first.UID] || !owners[second.UID] {
		t.Errorf("owners = %v, want both clusters", account.OwnerReferences)
	}
}

func TestTheWorkloadWaitsForTheClustersNames(t *testing.T) {
	cluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: secondCluster, Namespace: opsNamespace},
	}
	r := aNamesReconciler(t, cluster)

	key := client.ObjectKeyFromObject(cluster)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var services corev1.ServiceList
	if err := r.List(context.Background(), &services, client.InNamespace(opsNamespace)); err != nil {
		t.Fatalf("list the Services: %v", err)
	}
	if len(services.Items) != 0 {
		t.Errorf("%d Services were built before the cluster recorded its names", len(services.Items))
	}
}

// A supercluster has no storage nodes, and its Services are its own reconciler's
// (§6.2): a workload built for it would hold the names a standalone cluster in
// its namespace is reached by.
func TestTheWorkloadLeavesASuperclusterAlone(t *testing.T) {
	supercluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: opsNamespace},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
		Status: simplyblockv1alpha2.StorageClusterStatus{
			WorkloadNames: utils.WorkloadNamesStatus(atlaskube.DerivedWorkloadNames("prod")),
		},
	}
	r := aNamesReconciler(t, supercluster)

	key := client.ObjectKeyFromObject(supercluster)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var services corev1.ServiceList
	if err := r.List(context.Background(), &services, client.InNamespace(opsNamespace)); err != nil {
		t.Fatalf("list the Services: %v", err)
	}
	if len(services.Items) != 0 {
		t.Errorf("%d Services were built for a supercluster", len(services.Items))
	}
}

func TestANodeIsAddressedThroughItsClustersService(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(secondCluster)
	cluster := aNamedCluster(secondCluster, secondClusterUUID, names)
	r := aNamesReconciler(t, cluster)

	got := r.Workload.NodeAddress(context.Background(), opsNamespace, secondCluster, "worker-2.example")

	if want := "worker-2." + utils.ServiceFQDN(names.NodeAPIService, opsNamespace) + ":5000"; got != want {
		t.Errorf("NodeAddress = %q, want %q", got, want)
	}
}

func TestTheDaemonSetMountsTheClustersOwnCertificate(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(secondCluster)
	cluster := aNamedCluster(secondCluster, secondClusterUUID, names)
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{}

	ds := utils.BuildStorageNodeDaemonSet(cluster, true, false, "cert-manager", "", "image")

	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Name != "tls" {
			continue
		}
		if volume.Secret == nil || volume.Secret.SecretName != names.NodeAPITLSSecret {
			t.Errorf("the tls volume is %+v, want Secret %s", volume.VolumeSource, names.NodeAPITLSSecret)
		}
		return
	}
	t.Error("the DaemonSet mounts no tls volume")
}
