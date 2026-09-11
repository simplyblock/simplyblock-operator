// The scaffolding the band's tests share: a scheme, a fake client, and fake
// control planes.
//
// Everything here is deliberately small. The three reconcilers write Kubernetes
// objects and read a cache or an HTTP client, and both of those are interfaces
// on purpose, so a test drives a whole restore without a control plane and
// without an envtest.

package backup

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	testNamespace = "sb"
	testClusterCR = "production"
	testClusterID = "11111111-1111-1111-1111-111111111111"
	testPoolCR    = "pool-a"
	testPoolID    = "22222222-2222-2222-2222-222222222222"
	testLvolID    = "33333333-3333-3333-3333-333333333333"
	testBackupID  = "44444444-4444-4444-4444-444444444444"
	testRestoreID = "55555555-5555-5555-5555-555555555555"
)

func testScope() cpinformer.Scope { return cpinformer.Scope{testClusterID} }

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		scheme.AddToScheme,
		simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the scheme: %v", err)
		}
	}
	return s
}

// testClient builds a fake client carrying the index the mirror resolves a
// backup's source through. The fake client answers a MatchingFields query only
// for an index it was given, so the index has to be registered here as well as
// in SetupWithManager.
func testClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(
			&simplyblockv1alpha2.StorageBackup{},
			&simplyblockv1alpha2.StorageBackupPolicy{},
			&simplyblockv1alpha2.StorageBackupOps{},
		).
		WithIndex(&corev1.PersistentVolume{}, PersistentVolumeLvolIDIndex, IndexPersistentVolumeLvolID).
		WithObjects(objs...).
		Build()
}

func testRecorder() events.EventRecorder { return events.NewFakeRecorder(128) }

// objectMeta names an object in the band's test namespace.
func objectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: testNamespace}
}

func testClusterObject() *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: objectMeta(testClusterCR),
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: testClusterID},
	}
}

func testPoolObject() *simplyblockv1alpha1.StoragePool {
	return &simplyblockv1alpha1.StoragePool{
		ObjectMeta: objectMeta(testPoolCR),
		Spec:       simplyblockv1alpha1.StoragePoolSpec{ClusterName: testClusterCR},
		Status:     simplyblockv1alpha1.StoragePoolStatus{UUID: testPoolID},
	}
}

// fakeBackupCache is a static BackupCache for the mirror's tests.
type fakeBackupCache struct {
	synced  bool
	backups map[string]subscriptions.BackupDTO
}

func (f *fakeBackupCache) Triggers() <-chan event.GenericEvent { return nil }
func (f *fakeBackupCache) Synced(cpinformer.Scope) bool        { return f.synced }
func (f *fakeBackupCache) Lookup(
	key types.NamespacedName,
) (cpinformer.Scope, subscriptions.BackupDTO, bool) {
	dto, ok := f.backups[key.Name]
	if !ok {
		return nil, subscriptions.BackupDTO{}, false
	}
	return testScope(), dto, true
}

// fakeControlPlane answers the band's control-plane calls from fields a test
// sets, and counts what it was asked to do. The counts are what make an
// idempotence assertion possible: a step that ran twice and called once is the
// property under test.
type fakeControlPlane struct {
	policies    []controlplane.BackupPolicy
	createdID   string
	createErr   error
	attachErr   error
	detachErr   error
	restoredID  string
	restoreErr  error
	volumes     map[string]lvol.Volume
	volumeErr   error
	connection  lvol.Connection
	connectErr  error
	creates     int
	deletes     int
	attaches    []string
	detaches    []string
	restores    int
	volumeReads int

	// pool is what ListVolumes answers with, which is how a test puts a volume a
	// previous pass created into the world without a control plane.
	pool      []lvol.Volume
	listErr   error
	deleted   []string
	deleteErr error
}

func (f *fakeControlPlane) ListBackupPolicies(
	context.Context, string,
) ([]controlplane.BackupPolicy, error) {
	return f.policies, nil
}

func (f *fakeControlPlane) BackupPolicyByName(
	_ context.Context, _ string, name string,
) (controlplane.BackupPolicy, error) {
	for _, policy := range f.policies {
		if policy.Name == name {
			return policy, nil
		}
	}
	return controlplane.BackupPolicy{}, notFoundError{}
}

func (f *fakeControlPlane) CreateBackupPolicy(
	context.Context, string, controlplane.CreateBackupPolicyParams,
) (string, error) {
	f.creates++
	if f.createErr != nil {
		return "", f.createErr
	}
	return f.createdID, nil
}

func (f *fakeControlPlane) DeleteBackupPolicy(context.Context, string, string) error {
	f.deletes++
	return nil
}

func (f *fakeControlPlane) AttachBackupPolicy(_ context.Context, _, _, volumeID string) error {
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attaches = append(f.attaches, volumeID)
	return nil
}

func (f *fakeControlPlane) DetachBackupPolicy(_ context.Context, _, _, volumeID string) error {
	if f.detachErr != nil {
		return f.detachErr
	}
	f.detaches = append(f.detaches, volumeID)
	return nil
}

func (f *fakeControlPlane) RestoreBackup(
	context.Context, string, controlplane.RestoreBackupParams,
) (string, error) {
	f.restores++
	if f.restoreErr != nil {
		return "", f.restoreErr
	}
	return f.restoredID, nil
}

func (f *fakeControlPlane) Volume(_ context.Context, handle lvol.VolumeHandle) (lvol.Volume, error) {
	f.volumeReads++
	if f.volumeErr != nil {
		return lvol.Volume{}, f.volumeErr
	}
	return f.volumes[string(handle)], nil
}

func (f *fakeControlPlane) Connection(
	context.Context, lvol.VolumeHandle, ...lvol.ConnectionOption,
) (lvol.Connection, error) {
	return f.connection, f.connectErr
}

func (f *fakeControlPlane) ListVolumes(context.Context, string, string) ([]lvol.Volume, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.pool, nil
}

func (f *fakeControlPlane) DeleteVolume(_ context.Context, handle lvol.VolumeHandle) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, string(handle))
	return nil
}

// notFoundError satisfies errors.Is against the atlas sentinel the band checks
// for, without dragging the control plane's error plumbing into a fake.
type notFoundError struct{}

func (notFoundError) Error() string { return "not found" }
func (notFoundError) Is(target error) bool {
	return target != nil && target.Error() == "not found"
}
