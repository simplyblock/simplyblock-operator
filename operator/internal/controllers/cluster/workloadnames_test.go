// Tests for how a cluster comes by its workload names (design-superclusters.md
// §6.1): the constant names for a cluster created before the record and for one
// adopted from Helm, its own names for every new cluster, the names in the
// create body, and a control plane that does not keep them.

package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	atlascp "github.com/simplyblock/atlas/controlplane"
	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

func recordedNames(t *testing.T, cluster *simplyblockv1alpha2.StorageCluster) atlaskube.WorkloadNames {
	t.Helper()
	names, recorded := utils.WorkloadNamesOf(cluster)
	if !recorded {
		t.Fatalf("cluster %s has no workload names recorded (step %q, message %q)",
			cluster.Name, cluster.Status.Step.State, cluster.Status.Message)
	}
	return names
}

// otherCluster is a second cluster in the test namespace, named first, with
// the names it recorded.
func otherCluster(names atlaskube.WorkloadNames) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta("first"),
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(20)), VCPUCount: ptr.To(int32(8)),
		},
		Status: simplyblockv1alpha2.StorageClusterStatus{
			UUID:          "9b1d3f20-0000-4000-8000-000000000002",
			WorkloadNames: utils.WorkloadNamesStatus(names),
		},
	}
}

func createdWith(names atlascp.ClusterWorkloadNames) func(atlascp.CreateClusterParams) (atlascp.Cluster, error) {
	return func(atlascp.CreateClusterParams) (atlascp.Cluster, error) {
		reading := activeCluster()
		reading.Secret = testClusterSecret
		reading.WorkloadNames = names
		return reading, nil
	}
}

func TestAClusterCreatedBeforeTheRecordIsGivenTheConstantNames(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())

	cluster := reconcileCluster(t, r, 1)

	if got := recordedNames(t, cluster); !got.IsConstant() {
		t.Errorf("workloadNames = %+v, want the constant names", got)
	}
}

// A new cluster takes names of its own even when it is alone in its namespace,
// so that no cluster holds names the next one would have to avoid.
func TestANewClusterTakesItsOwnNames(t *testing.T) {
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{}, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 2)

	if got, want := recordedNames(t, cluster), atlaskube.DerivedWorkloadNames(testClusterName); got != want {
		t.Errorf("workloadNames = %+v, want %+v", got, want)
	}
}

// A cluster adopted from a Helm deployment is a new StorageCluster over a
// backend cluster that has run under the constant names all along, so it keeps
// them, and its adoption is not held for names the control plane never had.
func TestAClusterAdoptedFromHelmKeepsTheConstantNames(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	upgrade := &corev1.Secret{
		ObjectMeta: objectMeta("simplyblock-" + testClusterName + "-upgrade"),
		Data: map[string][]byte{
			"uuid":   []byte(testClusterUUID),
			"secret": []byte(testClusterSecret),
		},
	}
	r := newClusterReconciler(t, api, &recorder{}, newUncreatedCluster(), upgrade)

	cluster := reconcileCluster(t, r, 6)

	if got := recordedNames(t, cluster); !got.IsConstant() {
		t.Errorf("workloadNames = %+v, want the constant names", got)
	}
	if cluster.Status.UUID != testClusterUUID {
		t.Errorf("status.uuid = %q, want the adopted cluster (message %q)", cluster.Status.UUID, cluster.Status.Message)
	}
}

func TestASecondClusterInANamespaceTakesItsOwnNames(t *testing.T) {
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{},
		newUncreatedCluster(), otherCluster(atlaskube.ConstantWorkloadNames()))

	cluster := reconcileCluster(t, r, 2)

	if got, want := recordedNames(t, cluster), atlaskube.DerivedWorkloadNames(testClusterName); got != want {
		t.Errorf("workloadNames = %+v, want %+v", got, want)
	}
}

func TestTheCreateBodyCarriesTheWorkloadNames(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(testClusterName)
	var sent atlascp.CreateClusterParams
	api := &fakeControlPlane{
		create: func(params atlascp.CreateClusterParams) (atlascp.Cluster, error) {
			sent = params
			return createdWith(utils.BackendWorkloadNames(names))(params)
		},
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 6)

	if sent.WorkloadNames != utils.BackendWorkloadNames(names) {
		t.Errorf("create body names = %+v, want %+v", sent.WorkloadNames, utils.BackendWorkloadNames(names))
	}
	if cluster.Status.UUID != testClusterUUID {
		t.Errorf("status.uuid = %q, want the created cluster (message %q)", cluster.Status.UUID, cluster.Status.Message)
	}
}

// A control plane that predates the names accepts the create body and drops
// them. The cluster it creates is reached by the constant names, so a cluster
// recorded with its own names, which every new cluster is, would be unreachable
// there, and it is not finished.
func TestAControlPlaneThatDropsTheNamesHoldsTheCluster(t *testing.T) {
	api := &fakeControlPlane{
		forgetsNames: true,
		create:       createdWith(atlascp.ClusterWorkloadNames{}),
		cluster:      func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		// The cluster it created is listed, without the names, so a later
		// pass adopts it rather than creating another.
		byName: func(string) (atlascp.Cluster, bool, error) {
			return atlascp.Cluster{
				ID: testClusterUUID, Name: testClusterName, Status: utils.ClusterStatusActive,
				NDCS: 2, NPCS: 1,
			}, true, nil
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 6)

	if cluster.Status.UUID != "" {
		t.Errorf("status.uuid = %q on a cluster the control plane cannot reach by its names", cluster.Status.UUID)
	}
	if cluster.Status.Phase != simplyblockv1alpha2.StorageClusterPhaseUnavailable {
		t.Errorf("phase = %q, want Unavailable", cluster.Status.Phase)
	}
	if !rec.has(WorkloadNamesNotRecorded) {
		t.Error("no WorkloadNamesNotRecorded event says why the cluster is held")
	}
	if api.createCalls != 1 {
		t.Errorf("the control plane was asked to create %d clusters, want 1", api.createCalls)
	}
}

func TestASuperclusterRecordsItsOwnNames(t *testing.T) {
	supercluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta(testSuperclusterName),
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
	}
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{}, supercluster)

	key := types.NamespacedName{Namespace: testNamespace, Name: testSuperclusterName}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("read the supercluster back: %v", err)
	}

	if names, want := recordedNames(t, &got), atlaskube.DerivedWorkloadNames(testSuperclusterName); names != want {
		t.Errorf("workloadNames = %+v, want %+v", names, want)
	}
}

// A member is reached through its supercluster's Services with a suffix of its
// own (§6.2). It records those names from the start, because a record is never
// changed, and is the only cluster with storage nodes in its namespace until
// the supercluster builds the shared Services.
func TestAMemberRecordsItsSuperclustersNamesWithItsOwnSuffix(t *testing.T) {
	supercluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta(testSuperclusterName),
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
	}
	member := newUncreatedCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.SuperclusterRef = testSuperclusterName
	})
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{}, member, supercluster)

	cluster := reconcileCluster(t, r, 2)

	if got, want := recordedNames(t, cluster), atlaskube.MemberWorkloadNames(testSuperclusterName, testClusterName); got != want {
		t.Errorf("workloadNames = %+v, want %+v", got, want)
	}
}

// Every node's hostname is built from the record, so a reconcile never rewrites
// it, even when the rule that chose it would now choose otherwise: here the
// constant names are free again, and the cluster keeps its own.
func TestARecordedNameIsNeverRewritten(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(testClusterName)
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.WorkloadNames = utils.WorkloadNamesStatus(names)
	}))

	cluster := reconcileCluster(t, r, 3)

	if got := recordedNames(t, cluster); got != names {
		t.Errorf("workloadNames = %+v, want the recorded %+v", got, names)
	}
}

// The names leave the operator under the spec's keys, through the generated
// client, and are read back from the control plane's answer. The body of the
// cluster a control plane with sbcli#1512 answers with is the fixture.
func TestTheWorkloadNamesCrossTheWireUnderTheSpecsKeys(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(testClusterName)
	server := webapimock.NewSpecServerFromFile(t, clusterAdminSpecPath, false)
	defer server.Close()
	server.Register(http.MethodPost, "/api/v2/clusters/", webapimock.RouteResponse{
		Status:  http.StatusCreated,
		Headers: jsonContent,
		Body: `{"id":"` + testClusterUUID + `","nqn":"nqn.x","status":"active","distr_ndcs":2,"distr_npcs":1,` +
			`"snode_api_service":"` + names.NodeAPIService + `","spdk_proxy_service":"` + names.SpdkProxyService + `",` +
			`"spdk_proxy_tls_secret":"` + names.SpdkProxyTLSSecret + `","node_label_suffix":""}`,
	})
	api := NewControlPlane(func(context.Context) string { return server.URL() }, nil)

	created, err := api.CreateCluster(context.Background(), atlascp.CreateClusterParams{
		Name: testClusterName, MaxSubsys: 20, SpdkVCPUCount: 8, ClientDataNIC: "eth1",
		WorkloadNames: utils.BackendWorkloadNames(names),
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(server.Requests()[0].Body, &body); err != nil {
		t.Fatalf("decode the create body: %v", err)
	}
	for key, want := range map[string]string{
		"snode_api_service": names.NodeAPIService, "spdk_proxy_service": names.SpdkProxyService,
		"spdk_proxy_tls_secret": names.SpdkProxyTLSSecret, "client_data_nic": "eth1",
	} {
		if body[key] != want {
			t.Errorf("create body %q = %v, want %q", key, body[key], want)
		}
	}
	if !utils.BackendHolds(created.WorkloadNames, names) {
		t.Errorf("the answer's names %+v do not hold %+v", created.WorkloadNames, names)
	}
}
