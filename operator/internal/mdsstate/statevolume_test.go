// The state volume of a storage cluster's metadata server: created once, in
// the pool of the first export, recorded before any promotion, and deleted with
// the StatefulSet (design-pnfs-mds-standby.md §5.4, O-04 to O-06).

package mdsstate

import (
	"context"
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/lvol"
)

func testSTS(annotations map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "mds", Namespace: "simplyblock", UID: testUID, Annotations: annotations,
	}}
}

func kubeFor(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func reload(t *testing.T, c client.Client, sts *appsv1.StatefulSet) *appsv1.StatefulSet {
	t.Helper()
	var got appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &got); err != nil {
		t.Fatalf("reading the StatefulSet: %v", err)
	}
	return &got
}

// O-04: the volume is created once, 1 GiB, in the export's pool, and recorded.
func TestEnsureStateVolumeCreatesAndRecordsItOnce(t *testing.T) {
	api := newFakeAPI()
	sts := testSTS(nil)
	c := kubeFor(t, sts)
	sv := &StateVolume{Client: c, API: api}

	h, err := sv.Ensure(context.Background(), sts, testCluster, testPool)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got := reload(t, c, sts)
	if got.Annotations[StateVolumeAnnotation] != string(h) || h == "" {
		t.Errorf("recorded %q, want the created handle %q", got.Annotations[StateVolumeAnnotation], h)
	}
	if !slices.Contains(got.Finalizers, StateVolumeFinalizer) {
		t.Errorf("finalizers = %v, want %s", got.Finalizers, StateVolumeFinalizer)
	}
	v := api.volumes[h]
	if v == nil || v.vol.SizeBytes != StateVolumeSize || v.vol.Pool != testPool {
		t.Fatalf("volume = %+v, want %d bytes in pool %s", v, StateVolumeSize, testPool)
	}
	if StateVolumeSize != 1<<30 {
		t.Errorf("state volume size = %d, want 1 GiB", StateVolumeSize)
	}

	// A second pass, on the object as read back, creates nothing.
	if h2, err := sv.Ensure(context.Background(), got, testCluster, testPool); err != nil || h2 != h {
		t.Errorf("second Ensure = %q, %v, want %q", h2, err, h)
	}
	if api.creates != 1 {
		t.Errorf("%d create calls, want 1", api.creates)
	}
}

// A recorded handle wins: the recorded volume is returned even when the pool
// of the first export has changed, and nothing is created.
func TestEnsureStateVolumeKeepsTheRecordedHandle(t *testing.T) {
	api := newFakeAPI()
	recorded := api.add("whatever")
	sts := testSTS(map[string]string{StateVolumeAnnotation: string(recorded)})
	sv := &StateVolume{Client: kubeFor(t, sts), API: api}

	h, err := sv.Ensure(context.Background(), sts, testCluster, "99999999-2222-4333-8444-555555555555")
	if err != nil || h != recorded || api.creates != 0 {
		t.Errorf("Ensure = %q, %v with %d creates, want the recorded %q", h, err, api.creates, recorded)
	}
}

// The create succeeds and the record write is lost, as in a crash between the
// two. The next pass finds the volume by its deterministic name and records it,
// instead of creating a second one.
func TestEnsureStateVolumeAdoptsAVolumeCreatedBeforeACrash(t *testing.T) {
	api := newFakeAPI()
	sts := testSTS(nil)
	c := kubeFor(t, sts)
	sv := &StateVolume{Client: c, API: api}

	crash := errors.New("operator died")
	api.afterCreate = func() { api.createErr = nil }
	failing := &StateVolume{Client: &failingPatch{Client: c, err: crash}, API: api}
	if _, err := failing.Ensure(context.Background(), sts, testCluster, testPool); err == nil {
		t.Fatal("the lost record write was not reported")
	}
	if api.creates != 1 || reload(t, c, sts).Annotations[StateVolumeAnnotation] != "" {
		t.Fatalf("setup: %d creates, record %q", api.creates, reload(t, c, sts).Annotations[StateVolumeAnnotation])
	}

	h, err := sv.Ensure(context.Background(), reload(t, c, sts), testCluster, testPool)
	if err != nil {
		t.Fatalf("Ensure after the crash: %v", err)
	}
	if api.creates != 1 {
		t.Errorf("%d create calls, want the one volume adopted", api.creates)
	}
	if got := reload(t, c, sts).Annotations[StateVolumeAnnotation]; got != string(h) || h == "" {
		t.Errorf("recorded %q, want %q", got, h)
	}
}

// failingPatch loses every annotation write, as a crash before the record does.
type failingPatch struct {
	client.Client
	err error
}

func (f *failingPatch) Patch(ctx context.Context, obj client.Object, p client.Patch, o ...client.PatchOption) error {
	if obj.GetAnnotations()[StateVolumeAnnotation] != "" {
		return f.err
	}
	return f.Client.Patch(ctx, obj, p, o...)
}

// O-05: the control plane fails, nothing is recorded, and the error says the
// state volume is unavailable so the caller can emit MDSStateUnavailable.
func TestEnsureStateVolumeFailureRecordsNothing(t *testing.T) {
	api := newFakeAPI()
	api.createErr = errors.New("500 internal error")
	sts := testSTS(nil)
	c := kubeFor(t, sts)
	sv := &StateVolume{Client: c, API: api}

	h, err := sv.Ensure(context.Background(), sts, testCluster, testPool)
	if err == nil || h != "" {
		t.Fatalf("Ensure = %q, %v, want a failure", h, err)
	}
	if !errors.Is(err, ErrStateUnavailable) {
		t.Errorf("error %v is not ErrStateUnavailable", err)
	}
	if got := reload(t, c, sts).Annotations[StateVolumeAnnotation]; got != "" {
		t.Errorf("recorded %q after a failed create", got)
	}
}

// A pool named by a handle written before the v2 API is a name, resolved to its
// ID before the create.
func TestEnsureStateVolumeResolvesAPoolName(t *testing.T) {
	api := newFakeAPI()
	api.pools["fast"] = testPool
	sts := testSTS(nil)
	sv := &StateVolume{Client: kubeFor(t, sts), API: api}
	h, err := sv.Ensure(context.Background(), sts, testCluster, "fast")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got, _ := lvol.ParseHandle(h); got.PoolRef != testPool {
		t.Errorf("handle %q, want pool %s", h, testPool)
	}
}

// O-06: the finalizer deletes the volume and is removed. A 404 counts as done.
func TestFinalizeDeletesTheVolumeAndReleasesTheStatefulSet(t *testing.T) {
	api := newFakeAPI()
	h := api.add("state")
	now := metav1.Now()
	sts := testSTS(map[string]string{StateVolumeAnnotation: string(h)})
	sts.Finalizers = []string{StateVolumeFinalizer}
	sts.DeletionTimestamp = &now
	c := kubeFor(t, sts)
	sv := &StateVolume{Client: c, API: api}

	if err := sv.Finalize(context.Background(), sts); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if _, ok := api.volumes[h]; ok || api.deletes != 1 {
		t.Errorf("volume still exists or %d deletes", api.deletes)
	}
	var gone appsv1.StatefulSet
	err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &gone)
	if err == nil && slices.Contains(gone.Finalizers, StateVolumeFinalizer) {
		t.Errorf("finalizers = %v, want it removed", gone.Finalizers)
	}
}

func TestFinalizeCountsAMissingVolumeAsDone(t *testing.T) {
	api := newFakeAPI() // the volume was never there: the delete answers as for a 404
	now := metav1.Now()
	sts := testSTS(map[string]string{StateVolumeAnnotation: testCluster + ":" + testPool + ":00000000-0000-4000-8000-000000000099"})
	sts.Finalizers = []string{StateVolumeFinalizer}
	sts.DeletionTimestamp = &now
	c := kubeFor(t, sts)
	if err := (&StateVolume{Client: c, API: api}).Finalize(context.Background(), sts); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

// A failed delete keeps the finalizer, so the volume is not orphaned.
func TestFinalizeKeepsTheFinalizerWhenTheDeleteFails(t *testing.T) {
	api := newFakeAPI()
	h := api.add("state")
	api.deleteErr = errors.New("500")
	now := metav1.Now()
	sts := testSTS(map[string]string{StateVolumeAnnotation: string(h)})
	sts.Finalizers = []string{StateVolumeFinalizer}
	sts.DeletionTimestamp = &now
	c := kubeFor(t, sts)
	if err := (&StateVolume{Client: c, API: api}).Finalize(context.Background(), sts); err == nil {
		t.Fatal("a failed delete was reported as done")
	}
	if !slices.Contains(reload(t, c, sts).Finalizers, StateVolumeFinalizer) {
		t.Error("the finalizer was removed while the volume still exists")
	}
}
