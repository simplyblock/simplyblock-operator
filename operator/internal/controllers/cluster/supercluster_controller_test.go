// Tests for the supercluster reconciler (design-superclusters.md §10.1): it
// finds the members that name it, reads each member's capacity as that member,
// and writes the rollup, the aggregate identifier, and the events an
// administrator reads on the supercluster.

package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/simplyblock/atlas/prometheus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	testSuperclusterName = "prod"
	testSuperclusterUID  = "0d6f1b2e-1f0a-4c71-9a0e-5b7d2c3e4f50"
)

func newTestSupercluster(
	mutate ...func(*simplyblockv1alpha2.StorageCluster),
) *simplyblockv1alpha2.StorageCluster {
	c := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta(testSuperclusterName),
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			Supercluster: &simplyblockv1alpha2.SuperclusterSpec{},
		},
	}
	c.UID = types.UID(testSuperclusterUID)
	c.Generation = 3
	for _, m := range mutate {
		m(c)
	}
	return c
}

// newTestMember is an online member of the test supercluster with a backend
// UUID and a fault tolerance of one.
func newTestMember(
	name, uuid string, mutate ...func(*simplyblockv1alpha2.StorageCluster),
) *simplyblockv1alpha2.StorageCluster {
	c := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Name = name
		c.Spec.SuperclusterRef = testSuperclusterName
		c.Status.UUID = uuid
		c.Status.MaxFaultTolerance = ptr.To(int32(1))
	})
	for _, m := range mutate {
		m(c)
	}
	return c
}

func newSuperclusterReconciler(
	t *testing.T, api *fakeControlPlane, rec *recorder, objects ...client.Object,
) *SuperclusterReconciler {
	t.Helper()
	if api != nil {
		api.t = t
	}
	return &SuperclusterReconciler{
		Client:   newClient(t, objects...),
		Scheme:   testScheme(t),
		Recorder: rec,
	}
}

// exporterSamples is a metrics exporter answering with the samples given, by
// cluster UUID, or failing every read with err.
type exporterSamples struct {
	samples map[string]prometheus.Capacity
	err     error
}

func (e exporterSamples) ClusterCapacity(_ context.Context, uuid string) (prometheus.Capacity, bool, error) {
	if e.err != nil {
		return prometheus.Capacity{}, false, e.err
	}
	sample, ok := e.samples[uuid]
	return sample, ok, nil
}

// sample is an exporter reading of the given bytes.
func sample(total, used, provisioned int64) prometheus.Capacity {
	return prometheus.Capacity{Total: total, Used: used, Provisioned: provisioned, SampledAt: time.Unix(1, 0)}
}

// reconcileSupercluster runs one pass and returns the result and the
// supercluster as it stands afterward.
func reconcileSupercluster(
	t *testing.T, r *SuperclusterReconciler,
) (ctrl.Result, *simplyblockv1alpha2.StorageCluster) {
	t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testSuperclusterName}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var supercluster simplyblockv1alpha2.StorageCluster
	if err := r.Get(ctx, key, &supercluster); err != nil {
		t.Fatalf("read the supercluster back: %v", err)
	}
	return result, &supercluster
}

// I-01
func TestASuperclusterOfTwoOnlineMembersReportsOnline(t *testing.T) {
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{},
		newTestSupercluster(),
		newTestMember("prod-dense-kqv", "uuid-a"),
		newTestMember("prod-fast-mzt", "uuid-b"),
		// A standalone cluster and a member of another supercluster in the
		// same namespace are not this supercluster's members.
		newTestMember("staging-a-xyz", "uuid-c", func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.SuperclusterRef = "staging"
		}),
		newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) { c.Name = "standalone" }),
	)

	result, sc := reconcileSupercluster(t, r)

	status := sc.Status
	if status.Phase != simplyblockv1alpha2.StorageClusterPhaseOnline {
		t.Errorf("phase = %q, want Online (message: %s)", status.Phase, status.Message)
	}
	if status.Role != simplyblockv1alpha2.StorageClusterRoleSupercluster {
		t.Errorf("role = %q, want Supercluster", status.Role)
	}
	if status.UUID != testSuperclusterUID {
		t.Errorf("uuid = %q, want the object's uid %q", status.UUID, testSuperclusterUID)
	}
	if status.ObservedGeneration != 3 {
		t.Errorf("observedGeneration = %d, want 3", status.ObservedGeneration)
	}
	if status.MemberCount == nil || *status.MemberCount != 2 {
		t.Errorf("memberCount = %v, want 2", status.MemberCount)
	}
	for _, m := range status.Members {
		if !m.PlacementEligible {
			t.Errorf("member %s is not placement-eligible", m.Name)
		}
	}
	if status.MaxFaultTolerance == nil || *status.MaxFaultTolerance != 1 {
		t.Errorf("maxFaultTolerance = %v, want 1", status.MaxFaultTolerance)
	}
	if result.RequeueAfter != superclusterResync {
		t.Errorf("requeueAfter = %v, want the capacity poll interval %v",
			result.RequeueAfter, superclusterResync)
	}
}

// A member with no backend cluster yet has no capacity to read.
func TestAMemberWithNoUUIDIsNotAskedForCapacity(t *testing.T) {
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{},
		newTestSupercluster(),
		newTestMember("prod-dense-kqv", "", func(c *simplyblockv1alpha2.StorageCluster) {
			c.Status.Phase = simplyblockv1alpha2.StorageClusterPhaseCreating
		}))

	_, sc := reconcileSupercluster(t, r)

	if sc.Status.Phase != simplyblockv1alpha2.StorageClusterPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", sc.Status.Phase)
	}
}

// I-03
func TestAMemberGoingUnavailableDegradesTheSuperclusterAndLeavesPlacement(t *testing.T) {
	api := &fakeControlPlane{}
	rec := &recorder{}
	r := newSuperclusterReconciler(t, api, rec,
		newTestSupercluster(),
		newTestMember("prod-dense-kqv", "uuid-a"),
		newTestMember("prod-fast-mzt", "uuid-b"))
	reconcileSupercluster(t, r)

	var member simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Namespace: testNamespace, Name: "prod-fast-mzt"}
	if err := r.Get(context.Background(), key, &member); err != nil {
		t.Fatalf("read the member: %v", err)
	}
	member.Status.Phase = simplyblockv1alpha2.StorageClusterPhaseUnavailable
	if err := r.Status().Update(context.Background(), &member); err != nil {
		t.Fatalf("mark the member unavailable: %v", err)
	}

	_, sc := reconcileSupercluster(t, r)

	if sc.Status.Phase != simplyblockv1alpha2.StorageClusterPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", sc.Status.Phase)
	}
	for _, m := range sc.Status.Members {
		if m.Name == "prod-fast-mzt" && m.PlacementEligible {
			t.Error("the unavailable member is still placement-eligible")
		}
	}
	if rec.count(MemberPlacementChanged) != 1 {
		t.Errorf("%s events = %d, want 1", MemberPlacementChanged, rec.count(MemberPlacementChanged))
	}
	if typ := rec.typeFor(MemberPlacementChanged); typ != corev1.EventTypeNormal {
		t.Errorf("%s is %s, want Normal", MemberPlacementChanged, typ)
	}
}

// A member joining is announced once, on the pass that first sees it.
func TestAMemberJoiningIsAnnouncedOnce(t *testing.T) {
	api := &fakeControlPlane{}
	rec := &recorder{}
	r := newSuperclusterReconciler(t, api, rec,
		newTestSupercluster(), newTestMember("prod-dense-kqv", "uuid-a"))

	reconcileSupercluster(t, r)
	reconcileSupercluster(t, r)

	if rec.count(MemberAdded) != 1 {
		t.Errorf("%s events = %d, want 1", MemberAdded, rec.count(MemberAdded))
	}
}

// The supercluster's thresholds apply to the combined capacity, and a crossing
// is announced on the supercluster once rather than on every poll.
func TestTheCombinedCapacityCrossingAThresholdIsAnnouncedOnce(t *testing.T) {
	rec := &recorder{}
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, rec)
	sc := newTestSupercluster(func(c *simplyblockv1alpha2.StorageCluster) {
		c.Spec.WarningThreshold = &simplyblockv1alpha2.CapacityThresholdSpec{Capacity: ptr.To(int64(80))}
		c.Spec.CriticalThreshold = &simplyblockv1alpha2.CapacityThresholdSpec{Capacity: ptr.To(int64(90))}
	})

	r.announceThreshold(sc, reading(1000, 400, 0), reading(1000, 500, 0))
	if rec.has(AggregateCapacityThreshold) {
		t.Fatal("a threshold event at 50% of capacity")
	}

	r.announceThreshold(sc, reading(1000, 500, 0), reading(1000, 850, 0))
	r.announceThreshold(sc, reading(1000, 850, 0), reading(1000, 850, 0))
	if rec.count(AggregateCapacityThreshold) != 1 {
		t.Fatalf("%s events at 85%% = %d, want 1", AggregateCapacityThreshold, rec.count(AggregateCapacityThreshold))
	}
	if typ := rec.typeFor(AggregateCapacityThreshold); typ != corev1.EventTypeWarning {
		t.Errorf("%s is %s, want Warning", AggregateCapacityThreshold, typ)
	}

	r.announceThreshold(sc, reading(1000, 850, 0), reading(1000, 950, 0))
	if rec.count(AggregateCapacityThreshold) != 2 {
		t.Errorf("%s events after crossing critical = %d, want 2",
			AggregateCapacityThreshold, rec.count(AggregateCapacityThreshold))
	}
}

// The supercluster reconciler owns superclusters only. An ordinary cluster
// that reaches it, through a stale watch or a hand-made request, is left
// alone.
func TestTheSuperclusterReconcilerLeavesAnOrdinaryClusterAlone(t *testing.T) {
	standalone := newTestCluster(func(c *simplyblockv1alpha2.StorageCluster) { c.Name = testSuperclusterName })
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{}, standalone)

	_, after := reconcileSupercluster(t, r)
	if after.Status.Role != "" {
		t.Errorf("status.role = %q on an ordinary cluster, want it left to its own reconciler", after.Status.Role)
	}
}

// §6.2: a supercluster's members are reached through its Services, so the
// supercluster builds them, under its recorded names, owned by it, and asks
// for their serving certificates.
func TestASuperclusterBuildsTheServicesItsMembersShare(t *testing.T) {
	supercluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta(testSuperclusterName),
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
	}
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{}, supercluster)
	r.TLSEnabled, r.TLSProvider = true, "openshift"

	key := types.NamespacedName{Namespace: testNamespace, Name: testSuperclusterName}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("read the supercluster: %v", err)
	}
	names := atlaskube.DerivedWorkloadNames(testSuperclusterName)
	for service, secret := range map[string]string{
		names.NodeAPIService: names.NodeAPITLSSecret, names.SpdkProxyService: names.SpdkProxyTLSSecret,
	} {
		var object corev1.Service
		if err := r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: service}, &object); err != nil {
			t.Errorf("Service %s was not built: %v", service, err)
			continue
		}
		if owner := metav1.GetControllerOf(&object); owner == nil || owner.UID != got.UID {
			t.Errorf("Service %s is controlled by %+v, want the supercluster", service, owner)
		}
		if object.Annotations[utils.OpenShiftServingCertAnnotation] != secret {
			t.Errorf("Service %s asks for certificate %q, want %q",
				service, object.Annotations[utils.OpenShiftServingCertAnnotation], secret)
		}
	}
}

// The serving-certificate Secrets are written by the certificate issuer, which
// gives them no owner, so a supercluster takes them as its own once they exist:
// they go when it goes, instead of outliving it under names nothing uses.
func TestASuperclusterOwnsItsServingSecrets(t *testing.T) {
	supercluster := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: objectMeta(testSuperclusterName),
		Spec:       simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{}},
	}
	names := atlaskube.DerivedWorkloadNames(testSuperclusterName)
	issued := []string{names.NodeAPITLSSecret, names.SpdkProxyTLSSecret}
	objects := make([]client.Object, 0, 1+len(issued))
	objects = append(objects, supercluster)
	for _, name := range issued {
		objects = append(objects, &corev1.Secret{ObjectMeta: objectMeta(name)})
	}
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{}, objects...)
	r.TLSEnabled, r.TLSProvider = true, "openshift"

	key := types.NamespacedName{Namespace: testNamespace, Name: testSuperclusterName}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var owner simplyblockv1alpha2.StorageCluster
	if err := r.Get(context.Background(), key, &owner); err != nil {
		t.Fatalf("read the supercluster: %v", err)
	}
	for _, name := range issued {
		var secret corev1.Secret
		if err := r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &secret); err != nil {
			t.Fatalf("read Secret %s: %v", name, err)
		}
		owned := false
		for _, ref := range secret.OwnerReferences {
			owned = owned || ref.UID == owner.UID
		}
		if !owned {
			t.Errorf("Secret %s is owned by %+v, want the supercluster among its owners", name, secret.OwnerReferences)
		}
	}
}

// The supercluster reads no member's capacity from the control plane, which
// answers that read with a 500 for a member that has recorded no capacity yet,
// and without an exporter its capacity is left unreported.
func TestASuperclusterAsksTheControlPlaneForNoCapacity(t *testing.T) {
	api := &fakeControlPlane{}
	r := newSuperclusterReconciler(t, api, &recorder{},
		newTestSupercluster(), newTestMember("prod-dense-kqv", "uuid-a"))

	_, sc := reconcileSupercluster(t, r)

	if sc.Status.Capacity != nil {
		t.Errorf("capacity = %+v, want none", sc.Status.Capacity)
	}
}

// Each member's capacity is the exporter's latest sample of it, and the
// supercluster's is their sum. A member the exporter has no sample of
// contributes nothing.
func TestMemberCapacityIsReadFromTheExporter(t *testing.T) {
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{},
		newTestSupercluster(),
		newTestMember("prod-dense-kqv", "uuid-a"),
		newTestMember("prod-fast-mzt", "uuid-b"),
		newTestMember("prod-new-abc", "uuid-c"))
	r.Capacity = exporterSamples{samples: map[string]prometheus.Capacity{
		"uuid-a": sample(1000, 100, 400),
		"uuid-b": sample(2000, 300, 900),
	}}

	_, sc := reconcileSupercluster(t, r)

	if diff := cmp.Diff(reading(3000, 400, 1300), sc.Status.Capacity); diff != "" {
		t.Errorf("capacity (-want +got):\n%s", diff)
	}
	for _, m := range sc.Status.Members {
		if m.Name == "prod-new-abc" && m.Capacity != nil {
			t.Errorf("member %s has capacity %+v without a sample", m.Name, m.Capacity)
		}
	}
}

// I-07: an exporter that cannot be read keeps each member's last reading
// rather than dropping it from the sum.
func TestAnUnreadableExporterKeepsTheLastReading(t *testing.T) {
	last := reading(1000, 100, 400)
	r := newSuperclusterReconciler(t, &fakeControlPlane{}, &recorder{},
		newTestSupercluster(func(c *simplyblockv1alpha2.StorageCluster) {
			c.Status.Members = []simplyblockv1alpha2.SuperclusterMemberStatus{
				{Name: "prod-dense-kqv", UUID: "uuid-a", Capacity: last},
			}
		}),
		newTestMember("prod-dense-kqv", "uuid-a"))
	r.Capacity = exporterSamples{err: errors.New("prometheus is down")}

	_, sc := reconcileSupercluster(t, r)

	if diff := cmp.Diff(last, sc.Status.Capacity); diff != "" {
		t.Errorf("capacity after a failed read (-want +got):\n%s", diff)
	}
}
