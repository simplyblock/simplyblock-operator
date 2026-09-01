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
	"testing"

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
	return NewStorage(fakeVolumes{items: vols}, c)
}

func listIn(t *testing.T, s *Storage, namespace string, opts *metainternalversion.ListOptions) *metricsv1alpha1.LogicalVolumeMetricsList {
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

	list := listIn(t, s, testNamespace, nil)
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

	if n := len(listIn(t, s, "", nil).Items); n != 2 {
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

	list := listIn(t, s, "", nil)
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
	list := listIn(t, s, testNamespace, &metainternalversion.ListOptions{LabelSelector: sel})
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
	list := listIn(t, s, testNamespace, nil)

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
