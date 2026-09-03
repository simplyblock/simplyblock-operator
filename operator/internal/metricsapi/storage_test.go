// Tests for the LogicalVolumeMetrics REST storage: the join from a cached
// control-plane volume to the claim that names it, what happens to a volume
// that has no claim, and the selector and table behavior kubectl depends on.
//
// The correlation is the part worth testing. Everything else in this package is
// plumbing that fails loudly, but a wrong join is silent: it reports one
// tenant's capacity under another tenant's claim name, which is both a wrong
// answer and a disclosure.

package metricsapi

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/prometheus"

	metricsv1alpha1 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	testCluster = "11111111-1111-1111-1111-111111111111"
	testPool    = "22222222-2222-2222-2222-222222222222"
	testVolume  = "33333333-3333-3333-3333-333333333333"
	testVolumeB = "44444444-4444-4444-4444-444444444444"

	// The claim the happy path is built around, and the namespace it lives in.
	testClaim     = "postgres-data"
	testNamespace = "team-a"
)

// fakeVolumes is a VolumeSource over a fixed set, standing in for the
// subscription's live cache.
type fakeVolumes struct{ items []subscriptions.VolumeDTO }

func (f fakeVolumes) All() []subscriptions.VolumeDTO { return f.items }

func (f fakeVolumes) Get(id string) (subscriptions.VolumeDTO, bool) {
	for _, v := range f.items {
		if v.ID == id {
			return v, true
		}
	}
	return subscriptions.VolumeDTO{}, false
}

// fakeCapacity stands in for the metrics endpoint. It counts its calls, because
// one list must query a cluster once rather than once per volume.
type fakeCapacity struct {
	byCluster map[string]map[string]prometheus.Capacity
	err       error
	calls     int
}

func (f *fakeCapacity) VolumeCapacity(
	_ context.Context, clusterUUID string,
) (map[string]prometheus.Capacity, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byCluster[clusterUUID], nil
}

// capacityFrom turns the fixtures' own capacity blocks into samples, so that a
// case about listing, scoping, or table rendering says what a volume occupies
// without also restating which endpoint the number is read from. A case about
// that reads its own source in.
func capacityFrom(vols []subscriptions.VolumeDTO) *fakeCapacity {
	f := &fakeCapacity{byCluster: map[string]map[string]prometheus.Capacity{}}
	for _, v := range vols {
		if f.byCluster[v.ClusterID] == nil {
			f.byCluster[v.ClusterID] = map[string]prometheus.Capacity{}
		}
		c := prometheus.Capacity{
			Total:              v.Capacity.SizeTotal,
			Used:               v.Capacity.SizeUsed,
			Free:               v.Capacity.SizeFree,
			Provisioned:        v.Capacity.SizeProv,
			UtilizationPercent: v.Capacity.SizeUtil,
		}
		// The same rule the real source follows: a date of zero is no reading,
		// not a reading taken at the epoch.
		if v.Capacity.Date > 0 {
			c.SampledAt = time.Unix(v.Capacity.Date, 0).UTC()
		}
		f.byCluster[v.ClusterID][v.ID] = c
	}
	return f
}

func volume(id string, used, prov int64) subscriptions.VolumeDTO {
	return subscriptions.VolumeDTO{
		ID:        id,
		ClusterID: testCluster,
		PoolID:    testPool,
		PoolName:  "pool-a",
		Name:      "lvol-" + id[:8],
		Status:    "online",
		Size:      prov,
		Capacity: subscriptions.VolumeCapacityDTO{
			Date:      1756713600,
			SizeTotal: prov,
			SizeProv:  prov,
			SizeUsed:  used,
			SizeFree:  prov - used,
			SizeUtil:  int32(used * 100 / prov),
		},
	}
}

// pv builds a simplyblock-provisioned PersistentVolume bound to a claim.
func pv(name, volumeID, claimNS, claimName string) *corev1.PersistentVolume {
	p := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       kube.DriverName,
					VolumeHandle: testCluster + ":" + testPool + ":" + volumeID,
				},
			},
		},
	}
	if claimName != "" {
		p.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: claimNS, Name: claimName}
	}
	return p
}

func pvc(ns, name, volumeName string, lbls map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbls},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volumeName},
	}
}

func newStorage(t *testing.T, vols []subscriptions.VolumeDTO, objs ...client.Object) *Storage {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&corev1.PersistentVolume{}, kube.IndexPVByVolumeHandle, func(o client.Object) []string {
			return kube.VolumeHandleKeys(o.(*corev1.PersistentVolume))
		}).
		Build()
	return NewStorage(fakeVolumes{items: vols}, c, capacityFrom(vols))
}

func listFrom(t *testing.T, s *Storage, namespace string, opts *metainternalversion.ListOptions) *metricsv1alpha1.LogicalVolumeMetricsList {
	t.Helper()
	ctx := request.WithNamespace(context.Background(), namespace)
	if opts == nil {
		opts = &metainternalversion.ListOptions{}
	}
	obj, err := s.List(ctx, opts)
	if err != nil {
		t.Fatalf("List(%q): %v", namespace, err)
	}
	list, ok := obj.(*metricsv1alpha1.LogicalVolumeMetricsList)
	if !ok {
		t.Fatalf("List returned %T, want *LogicalVolumeMetricsList", obj)
	}
	return list
}

func TestListNamesObjectsAfterTheClaimAndScopesToItsNamespace(t *testing.T) {
	s := newStorage(t,
		[]subscriptions.VolumeDTO{volume(testVolume, 38654705664, 107374182400), volume(testVolumeB, 1024, 2048)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
		pv("pv-b", testVolumeB, "team-b", "redis-data"),
		pvc("team-b", "redis-data", "pv-b", nil),
	)

	list := listFrom(t, s, testNamespace, nil)
	if len(list.Items) != 1 {
		t.Fatalf("List(team-a) returned %d items, want 1", len(list.Items))
	}
	got := list.Items[0]
	if got.Name != testClaim || got.Namespace != testNamespace {
		t.Errorf("object identity = %s/%s, want team-a/postgres-data", got.Namespace, got.Name)
	}
	if got.PersistentVolume != "pv-a" {
		t.Errorf("PersistentVolume = %q, want pv-a", got.PersistentVolume)
	}
	if got.PoolName != "pool-a" {
		t.Errorf("PoolName = %q, want pool-a", got.PoolName)
	}
	if v := got.Capacity.Used.Value(); v != 38654705664 {
		t.Errorf("Capacity.Used = %d, want 38654705664", v)
	}
	if v := got.Capacity.Provisioned.Value(); v != 107374182400 {
		t.Errorf("Capacity.Provisioned = %d, want 107374182400", v)
	}
	if got.Capacity.UtilizationPercent != 36 {
		t.Errorf("UtilizationPercent = %d, want 36", got.Capacity.UtilizationPercent)
	}
	if got.VolumeHandle != testCluster+":"+testPool+":"+testVolume {
		t.Errorf("VolumeHandle = %q, want the three ids colon-separated", got.VolumeHandle)
	}
}

func TestListAcrossAllNamespaces(t *testing.T) {
	s := newStorage(t,
		[]subscriptions.VolumeDTO{volume(testVolume, 1024, 2048), volume(testVolumeB, 512, 2048)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
		pv("pv-b", testVolumeB, "team-b", "redis-data"),
		pvc("team-b", "redis-data", "pv-b", nil),
	)

	if n := len(listFrom(t, s, "", nil).Items); n != 2 {
		t.Errorf("cluster-wide List returned %d items, want 2", n)
	}
}

// A volume the cluster never provisioned through CSI has no claim to be named
// after and no namespace to be authorized against, so it is not served at all.
func TestListSkipsVolumesWithNoBoundClaim(t *testing.T) {
	s := newStorage(t,
		[]subscriptions.VolumeDTO{
			volume(testVolume, 1024, 2048), // has a PV and a claim
			volume(testVolumeB, 512, 2048), // no PV at all
		},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
		pv("pv-unbound", "55555555-5555-5555-5555-555555555555", "", ""),
	)

	list := listFrom(t, s, "", nil)
	if len(list.Items) != 1 {
		t.Fatalf("List returned %d items, want only the claim-backed one", len(list.Items))
	}
	if list.Items[0].Name != testClaim {
		t.Errorf("served %q, want postgres-data", list.Items[0].Name)
	}
}

// The claim's labels are copied onto the object precisely so that a selector
// means something; without them every selector would match nothing.
func TestListHonorsALabelSelectorOverTheClaimsLabels(t *testing.T) {
	s := newStorage(t,
		[]subscriptions.VolumeDTO{volume(testVolume, 1024, 2048), volume(testVolumeB, 512, 2048)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", map[string]string{"app": "postgres"}),
		pv("pv-b", testVolumeB, testNamespace, "redis-data"),
		pvc(testNamespace, "redis-data", "pv-b", map[string]string{"app": "redis"}),
	)

	sel, err := labels.Parse("app=postgres")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	list := listFrom(t, s, testNamespace, &metainternalversion.ListOptions{LabelSelector: sel})
	if len(list.Items) != 1 {
		t.Fatalf("selector matched %d items, want 1", len(list.Items))
	}
	if list.Items[0].Name != testClaim {
		t.Errorf("selector matched %q, want postgres-data", list.Items[0].Name)
	}
	if list.Items[0].Labels["app"] != "postgres" {
		t.Errorf("claim labels were not copied onto the object: %v", list.Items[0].Labels)
	}
}

func TestGetByClaimName(t *testing.T) {
	s := newStorage(t,
		[]subscriptions.VolumeDTO{volume(testVolume, 38654705664, 107374182400)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)

	ctx := request.WithNamespace(context.Background(), testNamespace)
	obj, err := s.Get(ctx, testClaim, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, ok := obj.(*metricsv1alpha1.LogicalVolumeMetrics)
	if !ok {
		t.Fatalf("Get returned %T, want *LogicalVolumeMetrics", obj)
	}
	if got.Name != testClaim || got.Namespace != testNamespace {
		t.Errorf("identity = %s/%s, want team-a/postgres-data", got.Namespace, got.Name)
	}
	if v := got.Capacity.Used.Value(); v != 38654705664 {
		t.Errorf("Capacity.Used = %d, want 38654705664", v)
	}
}

func TestGetIsNotFoundForAnUnknownClaim(t *testing.T) {
	s := newStorage(t, []subscriptions.VolumeDTO{volume(testVolume, 1024, 2048)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)

	ctx := request.WithNamespace(context.Background(), testNamespace)
	_, err := s.Get(ctx, "no-such-claim", &metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get(no-such-claim) error = %v, want a NotFound status error", err)
	}
}

// A claim bound to another vendor's volume is not this API's business, and
// answering for it would be a wrong answer rather than an empty one.
func TestGetIsNotFoundForAForeignDriversClaim(t *testing.T) {
	foreign := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-foreign"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-123"},
			},
			ClaimRef: &corev1.ObjectReference{Namespace: testNamespace, Name: "ebs-data"},
		},
	}
	s := newStorage(t, []subscriptions.VolumeDTO{volume(testVolume, 1024, 2048)},
		foreign, pvc(testNamespace, "ebs-data", "pv-foreign", nil))

	ctx := request.WithNamespace(context.Background(), testNamespace)
	_, err := s.Get(ctx, "ebs-data", &metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get on a foreign driver's claim = %v, want NotFound", err)
	}
}

// A claim whose volume the control plane has not reported yet is NotFound
// rather than an object with zeroed capacity, which would read as an empty
// volume.
func TestGetIsNotFoundWhenTheVolumeIsNotCached(t *testing.T) {
	s := newStorage(t, nil,
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)

	ctx := request.WithNamespace(context.Background(), testNamespace)
	if _, err := s.Get(ctx, testClaim, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get with a cold volume cache = %v, want NotFound", err)
	}
}

func TestStorageSurfaceMatchesWhatKubectlNeeds(t *testing.T) {
	s := newStorage(t, nil)
	if !s.NamespaceScoped() {
		t.Error("the resource must be namespaced; tenants are confined by namespace RBAC")
	}
	if got := s.ShortNames(); len(got) != 1 || got[0] != "lvm" {
		t.Errorf("ShortNames() = %v, want [lvm]", got)
	}
	if _, ok := s.New().(*metricsv1alpha1.LogicalVolumeMetrics); !ok {
		t.Errorf("New() = %T, want *LogicalVolumeMetrics", s.New())
	}
	if _, ok := s.NewList().(*metricsv1alpha1.LogicalVolumeMetricsList); !ok {
		t.Errorf("NewList() = %T, want *LogicalVolumeMetricsList", s.NewList())
	}
}

func TestConvertToTableRendersTheCapacityColumns(t *testing.T) {
	s := newStorage(t,
		[]subscriptions.VolumeDTO{volume(testVolume, 38654705664, 107374182400)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)
	list := listFrom(t, s, testNamespace, nil)

	table, err := s.ConvertToTable(context.Background(), list, nil)
	if err != nil {
		t.Fatalf("ConvertToTable: %v", err)
	}
	wantCols := []string{"Name", "Provisioned", "Used", "Used%", "Pool", "Volume Handle", "Age"}
	if len(table.ColumnDefinitions) != len(wantCols) {
		t.Fatalf("got %d columns, want %d", len(table.ColumnDefinitions), len(wantCols))
	}
	for i, want := range wantCols {
		if table.ColumnDefinitions[i].Name != want {
			t.Errorf("column %d = %q, want %q", i, table.ColumnDefinitions[i].Name, want)
		}
	}
	if len(table.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(table.Rows))
	}
	cells := table.Rows[0].Cells
	if cells[0] != testClaim {
		t.Errorf("name cell = %v, want postgres-data", cells[0])
	}
	if cells[2] != "36Gi" {
		t.Errorf("used cell = %v, want 36Gi", cells[2])
	}
	if cells[3] != "36%" {
		t.Errorf("utilization cell = %v, want 36%%", cells[3])
	}
}

// testVolumeSize is the nominal size every fixture volume is created with: the
// 10 GiB a claim asked for, which is what provisioned has to report.
const testVolumeSize = 10737418240

// asTheControlPlaneReportsIt builds the DTO shape a real control plane sends for
// a logical volume: a nominal size, and a capacity block of all zeros.
//
// The volume fixture above fills in both, which is more than the control plane
// does and is why no case in this file could fail for a field read from the
// wrong one of the two.
func asTheControlPlaneReportsIt(id string) subscriptions.VolumeDTO {
	return subscriptions.VolumeDTO{
		ID:        id,
		ClusterID: testCluster,
		PoolID:    testPool,
		PoolName:  "pool-a",
		Name:      "lvol-" + id[:8],
		Status:    "online",
		Size:      testVolumeSize,
		// Verified against the live API: every field of this block is zero for a
		// logical volume, including the sample date.
		Capacity: subscriptions.VolumeCapacityDTO{},
	}
}

// Regression: 2026-09-03-provisioned-reads-the-capacity-stat — provisioned was
// projected from the DTO's capacity.size_prov, which the control plane reports
// as zero for every logical volume, so a 10 GiB claim served a provisioned
// size of zero.
// The nominal size is carried in the DTO's own size field, which is required by
// the schema and correct on the wire.
func TestProvisionedIsTheVolumesNominalSizeNotACapacityStat(t *testing.T) {
	s := newStorage(t,
		[]subscriptions.VolumeDTO{asTheControlPlaneReportsIt(testVolume)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)

	list := listFrom(t, s, testNamespace, nil)
	if len(list.Items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(list.Items))
	}
	if got := list.Items[0].Capacity.Provisioned.Value(); got != testVolumeSize {
		t.Errorf("provisioned = %d, want the volume's nominal size %d", got, testVolumeSize)
	}
}

// newStorageWith builds the storage over an explicit capacity source, for the
// cases that are about the source itself rather than about what it reports.
func newStorageWith(
	t *testing.T,
	capacity CapacitySource,
	vols []subscriptions.VolumeDTO,
	objs ...client.Object,
) *Storage {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&corev1.PersistentVolume{}, kube.IndexPVByVolumeHandle, func(o client.Object) []string {
			return kube.VolumeHandleKeys(o.(*corev1.PersistentVolume))
		}).
		Build()
	return NewStorage(fakeVolumes{items: vols}, c, capacity)
}

// The measured fields come from the sample and not from the volume, which is
// the whole reason the source exists: the control plane's own capacity block
// reads zero for every logical volume.
func TestTheMeasuredFieldsComeFromTheCapacitySource(t *testing.T) {
	sampledAt := time.Unix(1788384958, 0).UTC()
	capacity := &fakeCapacity{byCluster: map[string]map[string]prometheus.Capacity{
		testCluster: {testVolume: {
			Total:              testVolumeSize,
			Used:               1080033280,
			Free:               9657384960,
			UtilizationPercent: 10,
			SampledAt:          sampledAt,
		}},
	}}

	s := newStorageWith(t, capacity,
		[]subscriptions.VolumeDTO{asTheControlPlaneReportsIt(testVolume)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)

	got := listFrom(t, s, testNamespace, nil).Items[0]
	if got.Capacity.Used.Value() != 1080033280 || got.Capacity.Free.Value() != 9657384960 {
		t.Errorf("used = %d, free = %d", got.Capacity.Used.Value(), got.Capacity.Free.Value())
	}
	if got.Capacity.Total.Value() != testVolumeSize || got.Capacity.UtilizationPercent != 10 {
		t.Errorf("total = %d, util = %d", got.Capacity.Total.Value(), got.Capacity.UtilizationPercent)
	}
	// The reading is as of when the control plane measured it, not when it was
	// served and not the epoch.
	if !got.Timestamp.Time.Equal(sampledAt) {
		t.Errorf("timestamp = %s, want the sample's %s", got.Timestamp, sampledAt)
	}
}

// A volume nobody has measured still has a size, and reporting it is more
// useful than omitting the volume. The measured fields stay zero and the
// timestamp stays unset, so a client can tell a fresh volume from a full one.
func TestAVolumeWithNoSampleStillReportsItsProvisionedSize(t *testing.T) {
	capacity := &fakeCapacity{byCluster: map[string]map[string]prometheus.Capacity{}}

	s := newStorageWith(t, capacity,
		[]subscriptions.VolumeDTO{asTheControlPlaneReportsIt(testVolume)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)

	got := listFrom(t, s, testNamespace, nil).Items[0]
	if got.Capacity.Provisioned.Value() != testVolumeSize {
		t.Errorf("provisioned = %d, want %d", got.Capacity.Provisioned.Value(), testVolumeSize)
	}
	if got.Capacity.Used.Value() != 0 {
		t.Errorf("used = %d, want 0 for a volume with no sample", got.Capacity.Used.Value())
	}
	if !got.Timestamp.IsZero() {
		t.Errorf("timestamp = %s, want unset rather than the epoch", got.Timestamp)
	}
}

// Prometheus is a dependency this API can answer partially without. A list that
// failed outright would tell a client nothing about which volumes it has, when
// the identities and the sizes are both still correct.
func TestAFailingCapacitySourceStillServesTheList(t *testing.T) {
	capacity := &fakeCapacity{err: errors.New("connection refused")}

	s := newStorageWith(t, capacity,
		[]subscriptions.VolumeDTO{asTheControlPlaneReportsIt(testVolume)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)

	list := listFrom(t, s, testNamespace, nil)
	if len(list.Items) != 1 {
		t.Fatalf("List returned %d items, want the volume anyway", len(list.Items))
	}
	if got := list.Items[0].Capacity.Provisioned.Value(); got != testVolumeSize {
		t.Errorf("provisioned = %d, want %d", got, testVolumeSize)
	}
	if got := list.Items[0].Capacity.Used.Value(); got != 0 {
		t.Errorf("used = %d, want 0 when no sample could be read", got)
	}
}

// A deployment with no Prometheus passes no source at all, which must not be a
// nil dereference on the read path.
func TestNoCapacitySourceServesProvisionedOnly(t *testing.T) {
	s := newStorageWith(t, nil,
		[]subscriptions.VolumeDTO{asTheControlPlaneReportsIt(testVolume)},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
	)

	got := listFrom(t, s, testNamespace, nil).Items[0]
	if got.Capacity.Provisioned.Value() != testVolumeSize {
		t.Errorf("provisioned = %d, want %d", got.Capacity.Provisioned.Value(), testVolumeSize)
	}
}

// One list is one query per cluster, not one per volume. Each query is an HTTP
// round trip on a request path, and every volume of a cluster is answered by
// the same one.
func TestAListQueriesEachClusterOnce(t *testing.T) {
	capacity := &fakeCapacity{byCluster: map[string]map[string]prometheus.Capacity{}}

	s := newStorageWith(t, capacity,
		[]subscriptions.VolumeDTO{
			asTheControlPlaneReportsIt(testVolume),
			asTheControlPlaneReportsIt(testVolumeB),
		},
		pv("pv-a", testVolume, testNamespace, testClaim),
		pvc(testNamespace, testClaim, "pv-a", nil),
		pv("pv-b", testVolumeB, testNamespace, "second-claim"),
		pvc(testNamespace, "second-claim", "pv-b", nil),
	)

	if n := len(listFrom(t, s, testNamespace, nil).Items); n != 2 {
		t.Fatalf("List returned %d items, want 2", n)
	}
	if capacity.calls != 1 {
		t.Errorf("queried the capacity source %d times for two volumes of one cluster, want 1",
			capacity.calls)
	}
}
