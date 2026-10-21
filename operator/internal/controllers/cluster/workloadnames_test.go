// Tests for how a cluster comes by its workload names (design-superclusters.md
// §6.1): the constant names for a namespace's first cluster and for one created
// before the record, its own names otherwise, the names in the create body, and
// a control plane that does not keep them.

package cluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
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

func createdWith(names webapi.BackendWorkloadNames) func(utils.ClusterAddParams) (webapi.ClusterResponse, error) {
	return func(utils.ClusterAddParams) (webapi.ClusterResponse, error) {
		reading := activeCluster()
		reading.Secret = testClusterSecret
		reading.BackendWorkloadNames = names
		return reading, nil
	}
}

func TestAClusterCreatedBeforeTheRecordIsGivenTheConstantNames(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newTestCluster())

	cluster := reconcileCluster(t, r, 1)

	if got := recordedNames(t, cluster); !got.IsConstant() {
		t.Errorf("workloadNames = %+v, want the constant names", got)
	}
}

func TestTheFirstClusterInANamespaceTakesTheConstantNames(t *testing.T) {
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{}, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 2)

	if got := recordedNames(t, cluster); !got.IsConstant() {
		t.Errorf("workloadNames = %+v, want the constant names", got)
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

// A cluster whose names are its own does not hold the constant names, so the
// next cluster may take them.
func TestTheConstantNamesAreFreeWhileNoClusterHoldsThem(t *testing.T) {
	r := newClusterReconciler(t, &fakeControlPlane{}, &recorder{},
		newUncreatedCluster(), otherCluster(atlaskube.DerivedWorkloadNames("first")))

	cluster := reconcileCluster(t, r, 2)

	if got := recordedNames(t, cluster); !got.IsConstant() {
		t.Errorf("workloadNames = %+v, want the constant names", got)
	}
}

// Two clusters created at once can both find the constant names unclaimed. The
// Service decides between them: the one that finds it held by the other before
// its backend cluster exists takes its own names instead.
func TestAClusterFindingTheConstantServiceHeldRewritesItsNamesBeforeCreating(t *testing.T) {
	winner := otherCluster(atlaskube.ConstantWorkloadNames())
	winner.UID = "uid-of-first"
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: atlaskube.ConstantNodeAPIService, Namespace: testNamespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: simplyblockv1alpha2.GroupVersion.String(), Kind: "StorageCluster",
			Name: "first", UID: winner.UID, Controller: ptr.To(true),
		}},
	}}
	// The race is lost before the names were read back: this cluster recorded
	// the constant names too, and has reached the step that creates it.
	loser := newUncreatedCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Status.WorkloadNames = utils.WorkloadNamesStatus(atlaskube.ConstantWorkloadNames())
		c.Status.Step.State = string(simplyblockv1alpha2.StorageClusterStepCreating)
	})
	api := &fakeControlPlane{}
	r := newClusterReconciler(t, api, &recorder{}, loser, winner, service)

	cluster := reconcileCluster(t, r, 1)

	if got, want := recordedNames(t, cluster), atlaskube.DerivedWorkloadNames(testClusterName); got != want {
		t.Errorf("workloadNames = %+v, want %+v", got, want)
	}
	if api.createCalls != 0 {
		t.Errorf("the cluster was created %d times under names another cluster holds", api.createCalls)
	}
}

func TestTheCreateBodyCarriesTheWorkloadNames(t *testing.T) {
	names := atlaskube.DerivedWorkloadNames(testClusterName)
	var sent utils.ClusterAddParams
	api := &fakeControlPlane{
		create: func(params utils.ClusterAddParams) (webapi.ClusterResponse, error) {
			sent = params
			return createdWith(utils.BackendWorkloadNames(names))(params)
		},
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{},
		newUncreatedCluster(), otherCluster(atlaskube.ConstantWorkloadNames()))

	cluster := reconcileCluster(t, r, 6)

	if sent.BackendWorkloadNames != utils.BackendWorkloadNames(names) {
		t.Errorf("create body names = %+v, want %+v", sent.BackendWorkloadNames, utils.BackendWorkloadNames(names))
	}
	if cluster.Status.UUID != testClusterUUID {
		t.Errorf("status.uuid = %q, want the created cluster (message %q)", cluster.Status.UUID, cluster.Status.Message)
	}
}

// A control plane that predates the names accepts the create body and drops
// them. The cluster it creates is reached by the constant names, so one recorded
// with its own names would be unreachable, and it is not finished.
func TestAControlPlaneThatDropsTheNamesHoldsTheCluster(t *testing.T) {
	api := &fakeControlPlane{
		create:  createdWith(webapi.BackendWorkloadNames{}),
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
		// The cluster it created is listed, without the names, so a later
		// pass adopts it rather than creating another.
		byName: func(string) (utils.ClusterListEntry, bool, error) {
			return utils.ClusterListEntry{
				UUID: testClusterUUID, Name: testClusterName, Status: utils.ClusterStatusActive,
				NDCS: 2, NPCS: 1,
			}, true, nil
		},
	}
	rec := &recorder{}
	r := newClusterReconciler(t, api, rec,
		newUncreatedCluster(), otherCluster(atlaskube.ConstantWorkloadNames()))

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

func TestAControlPlaneThatPredatesTheNamesStillCreatesTheFirstCluster(t *testing.T) {
	api := &fakeControlPlane{
		create:  createdWith(webapi.BackendWorkloadNames{}),
		cluster: func(string) (webapi.ClusterResponse, error) { return activeCluster(), nil },
	}
	r := newClusterReconciler(t, api, &recorder{}, newUncreatedCluster())

	cluster := reconcileCluster(t, r, 6)

	if cluster.Status.UUID != testClusterUUID {
		t.Errorf("status.uuid = %q, want the created cluster (message %q)", cluster.Status.UUID, cluster.Status.Message)
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
