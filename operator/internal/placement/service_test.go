// The placement service (design-superclusters.md §7.2, §7.3, §9.3): what it
// answers for a volume of a supercluster, what it records on the claim before
// answering, and how a retry, a burst, and a consistency group are answered.

package placement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/simplyblock/atlas/placement/placementrpc"
	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testNamespace = "simplyblock"
	aggregateID   = "0d6f1b2e-1f0a-4c71-9a0e-5b7d2c3e4f50"
	memberA       = "prod-a"
	memberB       = "prod-b"
	uuidA         = "4f0c0000-0000-4000-8000-00000000000a"
	uuidB         = "9b210000-0000-4000-8000-00000000000b"
	thePool       = "tenant-a"

	// orders is the consistency group the group tests label claims with.
	orders = "orders"
)

var epoch = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// fakeLookup answers the control-plane questions from tables.
type fakeLookup struct {
	mu      sync.Mutex
	volumes map[string]bool // member UUID + "/" + volume name
	groups  map[string]int  // member UUID + "/" + group name: live members
	failing string          // member UUID whose lookups fail
	asked   int
}

func (l *fakeLookup) VolumeExists(_ context.Context, member *simplyblockv1alpha2.StorageCluster, _, name string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked++
	if member.Status.UUID == l.failing {
		return false, errors.New("status 503")
	}
	return l.volumes[member.Status.UUID+"/"+name], nil
}

func (l *fakeLookup) GroupMembers(_ context.Context, member *simplyblockv1alpha2.StorageCluster, group string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if member.Status.UUID == l.failing {
		return 0, errors.New("status 503")
	}
	return l.groups[member.Status.UUID+"/"+group], nil
}

// fakeExporter answers capacity refreshes from a table, or fails them.
type fakeExporter struct {
	samples map[string]prometheus.Capacity
	fail    map[string]bool
	asked   int
}

func (e *fakeExporter) ClusterCapacity(_ context.Context, uuid string) (prometheus.Capacity, bool, error) {
	e.asked++
	if e.fail[uuid] {
		return prometheus.Capacity{}, false, errors.New("exporter unreachable")
	}
	s, ok := e.samples[uuid]
	return s, ok, nil
}

type eventSink struct {
	mu      sync.Mutex
	reasons []string
}

func (s *eventSink) Eventf(_ runtime.Object, _ runtime.Object, _, reason, _, _ string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons = append(s.reasons, reason)
}

func (s *eventSink) count(reason string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reasons {
		if r == reason {
			n++
		}
	}
	return n
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := simplyblockv1alpha2.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// supercluster is a supercluster of the given members, each eligible unless
// named in ineligible.
func supercluster(strategy simplyblockv1alpha2.PlacementStrategy, ineligible ...string) *simplyblockv1alpha2.StorageCluster {
	sc := &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: testNamespace, UID: aggregateID},
		Spec: simplyblockv1alpha2.StorageClusterSpec{Supercluster: &simplyblockv1alpha2.SuperclusterSpec{
			Placement: &simplyblockv1alpha2.SuperclusterPlacement{Strategy: strategy},
		}},
		Status: simplyblockv1alpha2.StorageClusterStatus{UUID: aggregateID},
	}
	for _, m := range []struct{ name, uuid string }{{memberA, uuidA}, {memberB, uuidB}} {
		eligible := true
		for _, n := range ineligible {
			eligible = eligible && n != m.name
		}
		sc.Status.Members = append(sc.Status.Members, simplyblockv1alpha2.SuperclusterMemberStatus{
			Name: m.name, UUID: m.uuid, Phase: simplyblockv1alpha2.StorageClusterPhaseOnline, PlacementEligible: eligible,
		})
	}
	return sc
}

func memberCluster(name, uuid string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       simplyblockv1alpha2.StorageClusterSpec{SuperclusterRef: "prod"},
		Status: simplyblockv1alpha2.StorageClusterStatus{
			UUID: uuid, Phase: simplyblockv1alpha2.StorageClusterPhaseOnline,
		},
	}
}

func pool() *simplyblockv1alpha2.StoragePool {
	return &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: thePool, Namespace: testNamespace},
		Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: "prod"},
		Status: simplyblockv1alpha2.StoragePoolStatus{Members: []simplyblockv1alpha2.PoolMemberStatus{
			{Cluster: memberA, UUID: "pool-a", Phase: simplyblockv1alpha2.StoragePoolPhaseReady},
			{Cluster: memberB, UUID: "pool-b", Phase: simplyblockv1alpha2.StoragePoolPhaseReady},
		}},
	}
}

func claim(name string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "app", Annotations: annotations,
	}}
}

type fixture struct {
	service  *Service
	lookup   *fakeLookup
	exporter *fakeExporter
	events   *eventSink
	client   client.Client
}

// newFixture builds a service over the objects, with both members read as
// 100 GiB with 50 GiB used a moment ago.
func newFixture(t *testing.T, funcs *interceptor.Funcs, objects ...client.Object) *fixture {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.StorageCluster{}, &simplyblockv1alpha2.StoragePool{})
	if funcs != nil {
		builder = builder.WithInterceptorFuncs(*funcs)
	}
	f := &fixture{
		lookup:   &fakeLookup{volumes: map[string]bool{}, groups: map[string]int{}},
		exporter: &fakeExporter{samples: map[string]prometheus.Capacity{}, fail: map[string]bool{}},
		events:   &eventSink{},
		client:   builder.Build(),
	}
	f.service = &Service{
		Client:   f.client,
		Lookup:   f.lookup,
		Capacity: f.exporter,
		Recorder: f.events,
		Now:      func() time.Time { return epoch },
		Draw:     func(int) int { return 0 },
	}
	for _, uuid := range []string{uuidA, uuidB} {
		f.service.Observe(uuid, prometheus.Capacity{Total: 100 * gib, Used: 50 * gib, SampledAt: epoch.Add(-time.Second)})
	}
	return f
}

func standardObjects(extra ...client.Object) []client.Object {
	return append([]client.Object{
		supercluster(simplyblockv1alpha2.PlacementMostAvailableCapacity),
		memberCluster(memberA, uuidA), memberCluster(memberB, uuidB), pool(),
	}, extra...)
}

func request(volume, claimName string) placementrpc.Request {
	return placementrpc.Request{
		AggregateID: aggregateID, PoolName: thePool, VolumeName: volume, SizeBytes: gib,
		ClaimName: claimName, ClaimNamespace: "app",
	}
}

// recordedOnTheClaim is the member the claim named data records.
func recordedOnTheClaim(t *testing.T, c client.Client) string {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "data"}, &pvc); err != nil {
		t.Fatal(err)
	}
	return pvc.Annotations[AnnotationPlacementMember]
}

// I-53: an answer comes from the supercluster's readings, and the chosen
// member is the handle's first component.
func TestPlaceAnswersAMembersBackendUUID(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	f.service.Observe(uuidB, prometheus.Capacity{Total: 100 * gib, Used: 10 * gib, SampledAt: epoch})

	member, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if err != nil || member != uuidB {
		t.Fatalf("Place = %q, %v; want %s, the emptier member", member, err, uuidB)
	}
}

// U-183: the chosen member is on the claim before Place answers.
func TestTheChoiceIsRecordedOnTheClaimBeforeTheAnswer(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)

	member, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if err != nil {
		t.Fatal(err)
	}
	if got := recordedOnTheClaim(t, f.client); got != member {
		t.Errorf("the claim records %q, want the answer %q", got, member)
	}
}

// U-183: a record that cannot be written fails the call with Unavailable, so
// a volume is never created where nothing says it was placed.
func TestARecordThatCannotBeWrittenFailsTheCall(t *testing.T) {
	refuse := &interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("etcd unavailable")
		},
	}
	f := newFixture(t, refuse, standardObjects(claim("data", nil))...)

	_, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if status.Code(err) != codes.Unavailable {
		t.Errorf("Place = %v, want Unavailable", err)
	}
}

// U-184: a retry for a claim carrying a record answers that member, even with
// the volume nowhere to be found yet and the member out of placement.
func TestARetryAnswersTheRecordedMember(t *testing.T) {
	objects := standardObjects(claim("data", map[string]string{AnnotationPlacementMember: uuidA}))
	objects[0] = supercluster(simplyblockv1alpha2.PlacementMostAvailableCapacity, memberA)
	f := newFixture(t, nil, objects...)

	member, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if err != nil || member != uuidA {
		t.Fatalf("Place = %q, %v; want the recorded %s", member, err, uuidA)
	}
	if f.lookup.asked != 0 {
		t.Errorf("the members were asked %d times, want none with a record", f.lookup.asked)
	}
}

// U-185: a request without its claim places nothing.
func TestARequestWithoutItsClaimIsRefused(t *testing.T) {
	f := newFixture(t, nil, standardObjects()...)

	_, err := f.service.Place(context.Background(), request("pvc-1", ""))

	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "extra-create-metadata") {
		t.Errorf("Place = %v, want FailedPrecondition naming --extra-create-metadata", err)
	}
}

// U-113, the operator's half: an identifier naming no supercluster is NotFound.
func TestAnUnknownAggregateIsNotFound(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	req := request("pvc-1", "data")
	req.AggregateID = "11111111-1111-4111-8111-111111111111"

	_, err := f.service.Place(context.Background(), req)

	if status.Code(err) != codes.NotFound {
		t.Errorf("Place = %v, want NotFound", err)
	}
}

// U-58: a volume a member already holds is answered with that member without
// ranking, even a member out of placement, and recorded.
func TestAVolumeAMemberHoldsIsAnsweredWithThatMember(t *testing.T) {
	objects := standardObjects(claim("data", nil))
	objects[0] = supercluster(simplyblockv1alpha2.PlacementMostAvailableCapacity, memberA)
	f := newFixture(t, nil, objects...)
	f.lookup.volumes[uuidA+"/pvc-1"] = true

	member, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if err != nil || member != uuidA {
		t.Fatalf("Place = %q, %v; want %s, which holds the volume", member, err, uuidA)
	}
	if got := recordedOnTheClaim(t, f.client); got != uuidA {
		t.Errorf("the claim records %q, want %s", got, uuidA)
	}
}

// U-60: a member that cannot be asked fails the call, because placing without
// knowing is how a duplicate arises.
func TestAMemberThatCannotBeAskedFailsTheCall(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	f.lookup.failing = uuidB

	_, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), memberB) {
		t.Errorf("Place = %v, want Unavailable naming %s", err, memberB)
	}
	if got := recordedOnTheClaim(t, f.client); got != "" {
		t.Errorf("the claim records %q, want nothing", got)
	}
}

// U-147: sizes placed since a member's reading count against it, so a burst
// of ten placements spreads over two equal members.
func TestABurstSpreadsOverEqualMembers(t *testing.T) {
	objects := standardObjects()
	for i := 0; i < 10; i++ {
		objects = append(objects, claim(fmt.Sprintf("data-%d", i), nil))
	}
	f := newFixture(t, nil, objects...)

	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		member, err := f.service.Place(context.Background(), request(fmt.Sprintf("pvc-%d", i), fmt.Sprintf("data-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		counts[member]++
	}
	if counts[uuidA] != 5 || counts[uuidB] != 5 {
		t.Errorf("placements = %v, want five in each member", counts)
	}
}

// A new reading clears what was placed before it, because the reading now
// accounts for it.
func TestANewReadingClearsWhatItIncludes(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data-0", nil), claim("data-1", nil))...)
	big := request("pvc-0", "data-0")
	big.SizeBytes = 40 * gib
	first, _ := f.service.Place(context.Background(), big)

	// A thin volume uses nothing yet, so the member's next reading is what
	// it was, and the 40 GiB no longer count against it.
	f.service.Observe(first, prometheus.Capacity{Total: 100 * gib, Used: 50 * gib, SampledAt: epoch.Add(time.Second)})
	f.service.Now = func() time.Time { return epoch.Add(2 * time.Second) }

	again := request("pvc-1", "data-1")
	again.SizeBytes = 40 * gib
	second, err := f.service.Place(context.Background(), again)
	if err != nil || second != first {
		t.Errorf("the second placement = %q, %v; want %q again, the tie's first", second, err, first)
	}
}

// U-150: two concurrent placements each see the size the other placed.
func TestConcurrentPlacementsSeeEachOther(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data-0", nil), claim("data-1", nil))...)

	var wg sync.WaitGroup
	answers := make([]string, 2)
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := request(fmt.Sprintf("pvc-%d", i), fmt.Sprintf("data-%d", i))
			req.SizeBytes = 40 * gib
			answers[i], _ = f.service.Place(context.Background(), req)
		}()
	}
	wg.Wait()

	if answers[0] == "" || answers[0] == answers[1] {
		t.Errorf("answers = %v, want two different members, each with 50 GiB free for 40 GiB", answers)
	}
}

// U-53: a reading older than two polling intervals is refreshed, and a member
// whose refresh fails is dropped while the other is still chosen.
func TestAStaleReadingIsRefreshedOrTheMemberDropped(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	later := epoch.Add(3 * time.Minute)
	f.service.Now = func() time.Time { return later }
	f.exporter.fail[uuidA] = true
	f.exporter.samples[uuidB] = prometheus.Capacity{Total: 100 * gib, Used: 90 * gib, SampledAt: later}

	member, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if err != nil || member != uuidB {
		t.Errorf("Place = %q, %v; want %s, refreshed, with the member whose refresh failed dropped",
			member, err, uuidB)
	}
	if f.exporter.asked != 2 {
		t.Errorf("refreshes = %d, want one per stale member", f.exporter.asked)
	}
}

// U-54: no member with room is ResourceExhausted naming each reason, and the
// supercluster hears of it once a minute.
func TestNoRoomIsExhaustedNamingEachReason(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil), claim("data-2", nil))...)
	req := request("pvc-1", "data")
	req.SizeBytes = 60 * gib

	_, err := f.service.Place(context.Background(), req)

	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), memberA) ||
		!strings.Contains(err.Error(), memberB) {
		t.Errorf("Place = %v, want ResourceExhausted naming both members", err)
	}
	again := request("pvc-2", "data-2")
	again.SizeBytes = 60 * gib
	_, _ = f.service.Place(context.Background(), again)
	if got := f.events.count(AggregateCapacityExhausted); got != 1 {
		t.Errorf("%s events = %d, want one per minute", AggregateCapacityExhausted, got)
	}
}

// U-172: under LeastVolumes a burst counts the volumes placed whose
// PersistentVolume does not exist yet, so it spreads.
func TestLeastVolumesCountsPlacementsWithoutAVolumeYet(t *testing.T) {
	objects := standardObjects(claim("data-0", nil), claim("data-1", nil))
	objects[0] = supercluster(simplyblockv1alpha2.PlacementLeastVolumes)
	f := newFixture(t, nil, objects...)

	first, _ := f.service.Place(context.Background(), request("pvc-0", "data-0"))
	second, _ := f.service.Place(context.Background(), request("pvc-1", "data-1"))

	if first == "" || second == "" || first == second {
		t.Errorf("placements = %q and %q, want one in each member", first, second)
	}
}

// LeastVolumes counts the PersistentVolumes whose handle names the member.
func TestLeastVolumesCountsTheMembersVolumes(t *testing.T) {
	objects := standardObjects(claim("data", nil))
	objects[0] = supercluster(simplyblockv1alpha2.PlacementLeastVolumes)
	objects = append(objects, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-existing"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.simplyblock.io", VolumeHandle: uuidA + ":pool-a:vol-1"},
		}},
	})
	f := newFixture(t, nil, objects...)

	member, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if err != nil || member != uuidB {
		t.Errorf("Place = %q, %v; want %s, which holds no volume", member, err, uuidB)
	}
}

// U-165: a labeled request whose group has live members in one member goes
// there, whatever the capacity ranking says.
func TestALabeledRequestFollowsItsGroup(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	f.service.Observe(uuidB, prometheus.Capacity{Total: 100 * gib, Used: 0, SampledAt: epoch})
	f.lookup.groups[uuidA+"/"+orders] = 2
	req := request("pvc-1", "data")
	req.ConsistencyGroup = orders

	member, err := f.service.Place(context.Background(), req)

	if err != nil || member != uuidA {
		t.Errorf("Place = %q, %v; want %s, which holds the group", member, err, uuidA)
	}
}

// U-166: a group in two members is refused, naming both.
func TestAGroupInTwoMembersIsRefused(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	f.lookup.groups[uuidA+"/"+orders] = 1
	f.lookup.groups[uuidB+"/"+orders] = 1
	req := request("pvc-1", "data")
	req.ConsistencyGroup = orders

	_, err := f.service.Place(context.Background(), req)

	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), memberA) ||
		!strings.Contains(err.Error(), memberB) {
		t.Errorf("Place = %v, want FailedPrecondition naming both members", err)
	}
}

// U-167: two first volumes of one group land in one member, because the
// second follows the service's record of the first.
func TestTheSecondFirstVolumeOfAGroupFollowsTheFirst(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data-0", nil), claim("data-1", nil))...)
	first := request("pvc-0", "data-0")
	first.ConsistencyGroup, first.SizeBytes = orders, 10*gib
	second := request("pvc-1", "data-1")
	second.ConsistencyGroup = orders

	a, _ := f.service.Place(context.Background(), first)
	b, err := f.service.Place(context.Background(), second)

	if err != nil || a != b {
		t.Errorf("placements = %q and %q (%v), want one member", a, b, err)
	}
}

// U-168: a group whose member is out of placement is refused, naming it.
func TestAGroupInAMemberOutOfPlacementIsRefused(t *testing.T) {
	objects := standardObjects(claim("data", nil))
	objects[0] = supercluster(simplyblockv1alpha2.PlacementMostAvailableCapacity, memberA)
	f := newFixture(t, nil, objects...)
	f.lookup.groups[uuidA+"/"+orders] = 1
	req := request("pvc-1", "data")
	req.ConsistencyGroup = orders

	_, err := f.service.Place(context.Background(), req)

	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), memberA) {
		t.Errorf("Place = %v, want FailedPrecondition naming %s", err, memberA)
	}
}

// A member's critical threshold comes from its own spec.
func TestAMembersCriticalThresholdIsItsOwn(t *testing.T) {
	objects := standardObjects(claim("data", nil))
	critical := memberCluster(memberB, uuidB)
	critical.Spec.CriticalThreshold = &simplyblockv1alpha2.CapacityThresholdSpec{Capacity: ptr.To(int64(40))}
	objects[2] = critical
	f := newFixture(t, nil, objects...)
	f.service.Observe(uuidB, prometheus.Capacity{Total: 100 * gib, Used: 45 * gib, SampledAt: epoch})

	member, err := f.service.Place(context.Background(), request("pvc-1", "data"))

	if err != nil || member != uuidA {
		t.Errorf("Place = %q, %v; want %s, with %s past its own 40%%", member, err, uuidA, memberB)
	}
}

// U-62, the operator's half: a clone through a supercluster class is answered
// with its source's member, whatever the ranking says, and recorded.
func TestACloneLandsInItsSourcesMember(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	f.service.Observe(uuidB, prometheus.Capacity{Total: 100 * gib, Used: 0, SampledAt: epoch})
	req := request("pvc-1", "data")
	req.SourceClusterID = uuidA

	member, err := f.service.Place(context.Background(), req)

	if err != nil || member != uuidA {
		t.Fatalf("Place = %q, %v; want the source's member %s", member, err, uuidA)
	}
	if got := recordedOnTheClaim(t, f.client); got != uuidA {
		t.Errorf("the claim records %q, want %s", got, uuidA)
	}
}

// U-63, the operator's half: a clone whose source is in no member of the
// class's supercluster is refused, because a clone cannot leave its source's
// cluster and the class promises the supercluster.
func TestACloneFromOutsideTheSuperclusterIsRefused(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	req := request("pvc-1", "data")
	req.SourceClusterID = "11111111-1111-4111-8111-111111111111"

	_, err := f.service.Place(context.Background(), req)

	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "prod") {
		t.Errorf("Place = %v, want InvalidArgument naming the supercluster", err)
	}
	if got := recordedOnTheClaim(t, f.client); got != "" {
		t.Errorf("the claim records %q, want nothing", got)
	}
}

// Choose places a volume the operator creates itself, a restore, among the
// members by the same ranking, and counts it against the member chosen, so a
// restore and a CreateVolume in one burst see each other.
func TestChooseRanksAndCountsTheVolume(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil))...)
	sc := supercluster(simplyblockv1alpha2.PlacementMostAvailableCapacity)

	first, err := f.service.Choose(context.Background(), sc, thePool, "restored-1", 40*gib)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	req := request("pvc-1", "data")
	req.SizeBytes = 40 * gib
	second, err := f.service.Place(context.Background(), req)

	if err != nil || first == "" || second == first {
		t.Errorf("Choose = %q, then Place = %q (%v); want two different members", first, second, err)
	}
}

// Choose with no member with room is ResourceExhausted, as Place is.
func TestChooseWithoutRoomIsExhausted(t *testing.T) {
	f := newFixture(t, nil, standardObjects()...)

	_, err := f.service.Choose(context.Background(), supercluster(simplyblockv1alpha2.PlacementMostAvailableCapacity),
		thePool, "restored-1", 60*gib)

	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("Choose = %v, want ResourceExhausted", err)
	}
}

// storageNodeOn is a storage node of the member running on the worker.
func storageNodeOn(name, member, worker string) *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: member, WorkerNode: worker},
	}
}

func workerIn(name, region string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		corev1.LabelTopologyRegion: region,
	}}}
}

// I-57: a member's region is read from its workers' Node labels at each
// placement, so a relabeled worker moves the member.
func TestAMembersRegionFollowsItsWorkers(t *testing.T) {
	f := newFixture(t, nil, standardObjects(claim("data", nil), claim("data-2", nil),
		storageNodeOn("sn-a", memberA, "worker-a"), storageNodeOn("sn-b", memberB, "worker-b"),
		workerIn("worker-a", "eu-west-1"), workerIn("worker-b", "eu-east-1"))...)
	f.service.Observe(uuidB, prometheus.Capacity{Total: 100 * gib, Used: 0, SampledAt: epoch})
	inWest := func(volume, claimName string) placementrpc.Request {
		req := request(volume, claimName)
		req.Preferred = []map[string]string{{corev1.LabelTopologyRegion: "eu-west-1"}}
		return req
	}

	if member, err := f.service.Place(context.Background(), inWest("pvc-1", "data")); err != nil || member != uuidA {
		t.Fatalf("Place = %q, %v; want %s, the member in eu-west-1", member, err, uuidA)
	}

	for name, region := range map[string]string{"worker-a": "eu-east-1", "worker-b": "eu-west-1"} {
		var worker corev1.Node
		if err := f.client.Get(context.Background(), types.NamespacedName{Name: name}, &worker); err != nil {
			t.Fatal(err)
		}
		worker.Labels[corev1.LabelTopologyRegion] = region
		if err := f.client.Update(context.Background(), &worker); err != nil {
			t.Fatal(err)
		}
	}

	if member, err := f.service.Place(context.Background(), inWest("pvc-2", "data-2")); err != nil || member != uuidB {
		t.Errorf("Place after relabeling = %q, %v; want %s, now in eu-west-1", member, err, uuidB)
	}
}
