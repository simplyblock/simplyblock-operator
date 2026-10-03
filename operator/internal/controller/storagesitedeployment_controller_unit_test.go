package controller

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workv1 "open-cluster-management.io/api/work/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testSDNamespace = "simplyblock"
	testSDName      = "site-a"
	testSDCluster   = "site-a"
)

func newStorageSiteDeploymentReconciler(t *testing.T, objects ...client.Object) (*StorageSiteDeploymentReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t)
	scheme.AddKnownTypeWithName(managedClusterViewGVK, &unstructured.Unstructured{})
	listGVK := managedClusterViewGVK
	listGVK.Kind += listKindSuffix
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	if err := workv1.Install(scheme); err != nil {
		t.Fatalf("register work/v1 scheme: %v", err)
	}
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha2.StorageSiteDeployment{}, &workv1.ManifestWork{}},
		objects...)
	return &StorageSiteDeploymentReconciler{Client: cl, Scheme: scheme, Recorder: &fakeRecorder{}}, cl
}

func newSiteDeployment(mutate func(*simplyblockv1alpha2.StorageSiteDeployment)) *simplyblockv1alpha2.StorageSiteDeployment {
	enable := true
	sd := &simplyblockv1alpha2.StorageSiteDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testSDName, Namespace: testSDNamespace, UID: types.UID("uid-site-a")},
		Spec: simplyblockv1alpha2.StorageSiteDeploymentSpec{
			Cluster:  testSDCluster,
			Discover: simplyblockv1alpha2.StorageSiteDiscovery{EnableControlPlaneNodes: &enable},
		},
	}
	if mutate != nil {
		mutate(sd)
	}
	return sd
}

// reconcileSD runs the reconcile n times (the first adds the finalizer) and
// returns the request as stored.
func reconcileSD(t *testing.T, r *StorageSiteDeploymentReconciler, cl client.Client, n int) *simplyblockv1alpha2.StorageSiteDeployment {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testSDNamespace, Name: testSDName}}
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	var sd simplyblockv1alpha2.StorageSiteDeployment
	if err := cl.Get(context.Background(), req.NamespacedName, &sd); err != nil {
		t.Fatalf("get request: %v", err)
	}
	return &sd
}

func getSiteWork(t *testing.T, cl client.Client, sd *simplyblockv1alpha2.StorageSiteDeployment) *workv1.ManifestWork {
	t.Helper()
	var w workv1.ManifestWork
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: sd.Spec.Cluster, Name: workName(sd)}, &w); err != nil {
		t.Fatalf("get ManifestWork: %v", err)
	}
	return &w
}

// workManifests decodes the work's manifests by kind.
func workManifests(t *testing.T, w *workv1.ManifestWork) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, m := range w.Spec.Workload.Manifests {
		obj := map[string]any{}
		if err := json.Unmarshal(m.Raw, &obj); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		out[obj["kind"].(string)] = obj
	}
	return out
}

// projectSiteObject writes what the site's view controller would project.
func projectSiteObject(t *testing.T, cl client.Client, sd *simplyblockv1alpha2.StorageSiteDeployment, suffix string, obj any) {
	t.Helper()
	v := getView(t, cl, sd.Spec.Cluster, storageSiteViewName(sd, suffix))
	result, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("convert projection: %v", err)
	}
	setViewResult(t, cl, v, result)
}

func siteDraft(approved bool, phase simplyblockv1alpha2.ClusterDeploymentConfigPhase, mutate func(*simplyblockv1alpha2.ClusterDeploymentConfig)) *simplyblockv1alpha2.ClusterDeploymentConfig {
	cdc := &simplyblockv1alpha2.ClusterDeploymentConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: simplyblockv1alpha2.GroupVersion.String(), Kind: "ClusterDeploymentConfig"},
		ObjectMeta: metav1.ObjectMeta{Name: "site-draft", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
			Approved: approved,
			NodeSets: []simplyblockv1alpha2.NodeSet{{
				Name:   "default",
				Groups: []simplyblockv1alpha2.NodeGroup{{Workers: []string{"n1", "n2", "n3"}}},
			}},
		},
	}
	cdc.Status.Phase = phase
	if mutate != nil {
		mutate(cdc)
	}
	return cdc
}

func TestStorageSiteDeploymentDeliversTheDiscoveryAndWaitsForTheDraft(t *testing.T) {
	r, cl := newStorageSiteDeploymentReconciler(t, newSiteDeployment(nil))
	sd := reconcileSD(t, r, cl, 2)

	if sd.Status.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseDiscovering {
		t.Fatalf("phase = %q, want Discovering", sd.Status.Phase)
	}
	w := getSiteWork(t, cl, sd)
	if w.Spec.DeleteOption == nil || w.Spec.DeleteOption.PropagationPolicy != workv1.DeletePropagationPolicyTypeOrphan {
		t.Errorf("work delete option = %+v, want Orphan: a request must never take the site's storage away", w.Spec.DeleteOption)
	}
	ms := workManifests(t, w)
	ops, ok := ms["OperatorOps"]
	if !ok || len(ms) != 1 {
		t.Fatalf("manifests = %v, want the discovery alone before the draft exists", keys(ms))
	}
	discover := ops["spec"].(map[string]any)["discover"].(map[string]any)
	if discover["configName"] != "site-draft" || discover["enableControlPlaneNodes"] != true {
		t.Errorf("discover = %v, want configName site-draft and control-plane nodes enabled", discover)
	}
	// The draft view exists so the site can project the draft.
	getView(t, cl, testSDCluster, storageSiteViewName(sd, "draft"))
}

func TestStorageSiteDeploymentSizesTheDraftOnceItNamesNodes(t *testing.T) {
	vcpu := int32(8)
	data, parity := int32(1), int32(1)
	r, cl := newStorageSiteDeploymentReconciler(t, newSiteDeployment(func(sd *simplyblockv1alpha2.StorageSiteDeployment) {
		sd.Spec.Sizing = &simplyblockv1alpha2.StorageSiteSizing{
			Name: "sb-site-a", VCPUCount: &vcpu, MinHugePagesSize: "8G",
			Stripe: &simplyblockv1alpha2.StripeSpec{DataChunks: &data, ParityChunks: &parity},
		}
	}))
	sd := reconcileSD(t, r, cl, 2)
	projectSiteObject(t, cl, sd, "draft", siteDraft(false, simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft, nil))
	sd = reconcileSD(t, r, cl, 2)

	if sd.Status.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseDrafted {
		t.Fatalf("phase = %q (%s), want Drafted", sd.Status.Phase, sd.Status.Message)
	}
	if got := draftNodeCount(sd.Status.Draft); got != 3 {
		t.Errorf("projected draft nodes = %d, want 3", got)
	}
	ms := workManifests(t, getSiteWork(t, cl, sd))
	cdc, ok := ms["ClusterDeploymentConfig"]
	if !ok {
		t.Fatalf("manifests = %v, want the draft's sizing applied", keys(ms))
	}
	spec := cdc["spec"].(map[string]any)
	if spec["approved"] != false {
		t.Errorf("approved = %v, want false before the request is approved", spec["approved"])
	}
	cluster := spec["cluster"].(map[string]any)
	if cluster["name"] != "sb-site-a" || cluster["vcpuCount"] != float64(8) || cluster["minHugePagesSize"] != "8G" {
		t.Errorf("cluster template = %v, want the sizing", cluster)
	}
	if _, ok := spec["nodeSets"]; ok {
		t.Error("the sizing apply must not carry nodeSets: they are the discovery's")
	}
	if !meta.IsStatusConditionTrue(sd.Status.Conditions, ConditionStorageSiteDiscovered) {
		t.Error("Discovered condition is not True for a draft with nodes")
	}
}

func TestStorageSiteDeploymentApprovalFollowsTheClusterToOnline(t *testing.T) {
	r, cl := newStorageSiteDeploymentReconciler(t, newSiteDeployment(func(sd *simplyblockv1alpha2.StorageSiteDeployment) {
		sd.Spec.Approved = true
	}))
	sd := reconcileSD(t, r, cl, 2)
	projectSiteObject(t, cl, sd, "draft", siteDraft(false, simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft, nil))
	// One pass projects the draft, the next delivers the approval onto it.
	sd = reconcileSD(t, r, cl, 2)

	ms := workManifests(t, getSiteWork(t, cl, sd))
	if ms["ClusterDeploymentConfig"]["spec"].(map[string]any)["approved"] != true {
		t.Fatalf("the work does not carry the approval: %v", ms["ClusterDeploymentConfig"])
	}
	if sd.Status.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseDrafted {
		t.Fatalf("phase = %q, want Drafted until the site's draft takes the approval", sd.Status.Phase)
	}

	// The site's draft takes it and starts expanding.
	projectSiteObject(t, cl, sd, "draft", siteDraft(true, simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding, func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Status.ClusterRef = "sb-site-a"
		c.Status.NodeRefs = []string{"sn-1", "sn-2", "sn-3"}
	}))
	sd = reconcileSD(t, r, cl, 1)
	if sd.Status.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseDeploying {
		t.Fatalf("phase = %q (%s), want Deploying", sd.Status.Phase, sd.Status.Message)
	}

	// The cluster comes Online.
	sc := &simplyblockv1alpha2.StorageCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: simplyblockv1alpha2.GroupVersion.String(), Kind: "StorageCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "sb-site-a", Namespace: "simplyblock"},
	}
	sc.Status.UUID = "8f8dd277-1544-4177-9a74-e0f66eb2672c"
	sc.Status.Phase = simplyblockv1alpha2.StorageClusterPhaseOnline
	projectSiteObject(t, cl, sd, "cluster", sc)
	sd = reconcileSD(t, r, cl, 1)

	if sd.Status.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseOnline {
		t.Fatalf("phase = %q (%s), want Online", sd.Status.Phase, sd.Status.Message)
	}
	if sd.Status.StorageCluster == nil || sd.Status.StorageCluster.UUID != sc.Status.UUID {
		t.Fatalf("storageCluster = %+v, want the site's uuid", sd.Status.StorageCluster)
	}
	if sd.Status.StorageCluster.Pool == "" {
		t.Error("storageCluster.pool is empty: a StorageClass needs it")
	}
	if n := len(sd.Status.StorageCluster.Nodes); n != 3 {
		t.Errorf("projected nodes = %d, want 3", n)
	}
	if !meta.IsStatusConditionTrue(sd.Status.Conditions, ConditionStorageSiteReady) {
		t.Error("Ready condition is not True for an Online cluster")
	}
}

func TestStorageSiteDeploymentReportsAFailedDraft(t *testing.T) {
	r, cl := newStorageSiteDeploymentReconciler(t, newSiteDeployment(func(sd *simplyblockv1alpha2.StorageSiteDeployment) {
		sd.Spec.Approved = true
	}))
	sd := reconcileSD(t, r, cl, 2)
	projectSiteObject(t, cl, sd, "draft", siteDraft(true, simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed, func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Status.Message = "node n2 has no free device"
	}))
	sd = reconcileSD(t, r, cl, 1)
	if sd.Status.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseFailed {
		t.Fatalf("phase = %q, want Failed", sd.Status.Phase)
	}
	if sd.Status.Message != "the site's draft failed: node n2 has no free device" {
		t.Errorf("message = %q, want the site's own reason", sd.Status.Message)
	}
}

func TestStorageSiteDeploymentADraftWithoutNodesIsStillDiscovering(t *testing.T) {
	r, cl := newStorageSiteDeploymentReconciler(t, newSiteDeployment(func(sd *simplyblockv1alpha2.StorageSiteDeployment) {
		sd.Spec.Approved = true
	}))
	sd := reconcileSD(t, r, cl, 2)
	projectSiteObject(t, cl, sd, "draft", siteDraft(false, simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft, func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.NodeSets = nil
	}))
	sd = reconcileSD(t, r, cl, 1)
	if sd.Status.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseDiscovering {
		t.Fatalf("phase = %q, want Discovering", sd.Status.Phase)
	}
	if _, ok := workManifests(t, getSiteWork(t, cl, sd))["ClusterDeploymentConfig"]; ok {
		t.Error("the approval was delivered onto a draft without nodes")
	}
}

func TestStorageSiteDeploymentAChangedDiscoveryIsANewRun(t *testing.T) {
	a := newSiteDeployment(nil)
	b := newSiteDeployment(func(sd *simplyblockv1alpha2.StorageSiteDeployment) {
		sd.Spec.Discover.Workers = []string{"n1"}
	})
	if discoveryName(a) == discoveryName(b) {
		t.Fatalf("discovery name %q did not change with the discovery's parameters", discoveryName(a))
	}
	if discoveryName(a) != discoveryName(newSiteDeployment(nil)) {
		t.Fatal("the discovery name is not stable for the same parameters")
	}
}

func TestStorageSiteDeploymentDeletionOrphansTheSitesStorage(t *testing.T) {
	r, cl := newStorageSiteDeploymentReconciler(t, newSiteDeployment(nil))
	sd := reconcileSD(t, r, cl, 2)
	if err := cl.Delete(context.Background(), sd); err != nil {
		t.Fatalf("delete request: %v", err)
	}
	// First pass deletes the work; the fake client removes it at once.
	reconcileOnce := func() {
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testSDNamespace, Name: testSDName}}
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	reconcileOnce()
	reconcileOnce()

	var w workv1.ManifestWork
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testSDCluster, Name: workName(sd)}, &w); err == nil {
		t.Error("the work is still there after the request was deleted")
	}
	views := &unstructured.UnstructuredList{}
	listGVK := managedClusterViewGVK
	listGVK.Kind += listKindSuffix
	views.SetGroupVersionKind(listGVK)
	if err := cl.List(context.Background(), views, client.InNamespace(testSDCluster)); err != nil {
		t.Fatalf("list views: %v", err)
	}
	if len(views.Items) != 0 {
		t.Errorf("%d view(s) left after the request was deleted", len(views.Items))
	}
	var gone simplyblockv1alpha2.StorageSiteDeployment
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(sd), &gone); err == nil {
		t.Error("the request is still there: the finalizer was not released")
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
