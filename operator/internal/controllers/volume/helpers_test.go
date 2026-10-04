// The scaffolding this package's unit tests share: a scheme, a fake client, a
// fake control plane, and the objects a migration needs to exist.
//
// The envtest apiserver in suite_test.go is only for the CRD's CEL rules.
// Everything else here runs against a fake client, which is what makes a whole
// migration drivable without a control plane and without a cluster.

package volume

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testNamespace   = "simplyblock"
	testClusterCR   = "production"
	testClusterID   = "11111111-1111-1111-1111-111111111111"
	testPoolID      = "22222222-2222-2222-2222-222222222222"
	testVolumeID    = "33333333-3333-3333-3333-333333333333"
	testMigrationID = "44444444-4444-4444-4444-444444444444"
	testTargetID    = "55555555-5555-5555-5555-555555555555"
	testSourceID    = "66666666-6666-6666-6666-666666666666"

	testPVName     = "pvc-" + testVolumeID
	testTargetNode = "worker-5"
	testNQN        = "nqn.2023-02.io.simplyblock:" + testClusterID + ":lvol:" + testVolumeID
	testOpsName    = "move-1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		k8sscheme.AddToScheme,
		simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the scheme: %v", err)
		}
	}
	return s
}

func testClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(
			&simplyblockv1alpha2.PersistentVolumeOps{},
			&simplyblockv1alpha2.StorageCluster{},
			&simplyblockv1alpha2.StorageNode{},
		).
		WithObjects(objs...).
		Build()
}

// testReconciler wires a reconciler onto a fake world. The API is the fake
// control plane, so every step is drivable without an HTTP server.
func testReconciler(t *testing.T, api MigrationClient, objs ...client.Object) *PersistentVolumeOpsReconciler {
	t.Helper()
	c := testClient(t, objs...)
	return &PersistentVolumeOpsReconciler{
		Client:   c,
		Reader:   c,
		Scheme:   testScheme(t),
		Recorder: events.NewFakeRecorder(256),
		API:      api,
	}
}

// testOperation is a well-formed migration of the test volume to the test node.
func testOperation() *simplyblockv1alpha2.PersistentVolumeOps {
	return &simplyblockv1alpha2.PersistentVolumeOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testOpsName,
			UID:        types.UID("uid-" + testOpsName),
			Finalizers: []string{opsFinalizer},
		},
		Spec: simplyblockv1alpha2.PersistentVolumeOpsSpec{
			PersistentVolumeName: testPVName,
			Action:               simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
			Migrate: &simplyblockv1alpha2.MigrateVolumeSpec{
				TargetNodeRef: simplyblockv1alpha2.StorageNodeReference{
					Namespace: testNamespace,
					Name:      testTargetNode,
				},
			},
		},
	}
}

// testVolumeObject is the PersistentVolume the operation acts on, provisioned
// by this driver and carrying the handle the cluster, pool, and volume are all
// read out of.
func testVolumeObject() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: testPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       CSIDriverName,
					VolumeHandle: string(lvol.NewVolumeHandle(testClusterID, testPoolID, testVolumeID)),
				},
			},
		},
	}
}

func testClusterObject() *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterCR, Namespace: testNamespace},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: testClusterID},
	}
}

func testNodeObject() *simplyblockv1alpha2.StorageNode {
	return &simplyblockv1alpha2.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: testTargetNode, Namespace: testNamespace},
		Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: testClusterCR},
		Status: simplyblockv1alpha2.StorageNodeStatus{
			UUID:  testTargetID,
			Phase: simplyblockv1alpha2.StorageNodePhaseOnline,
		},
	}
}

// fakeControlPlane answers the migration calls from fields a test sets, and
// counts what it was asked to do. The counts are what make an idempotence
// assertion possible: a step that ran twice and called once is the property
// under test.
type fakeControlPlane struct {
	volume    lvol.Volume
	volumeErr error

	members    []lvol.Volume
	membersErr error

	created     controlplane.Migration
	createErr   error
	creates     int
	continues   int
	cancels     int
	continueErr error
	cancelErr   error

	// read is what GetMigration answers with, which is how a test drives the
	// copy forward: the step completes on the migration's reported state
	// rather than on the return of the call that started it.
	read    controlplane.Migration
	readErr error
	reads   int
}

func (f *fakeControlPlane) Volume(context.Context, lvol.VolumeHandle) (lvol.Volume, error) {
	return f.volume, f.volumeErr
}

func (f *fakeControlPlane) SubsystemVolumes(context.Context, string, string) ([]lvol.Volume, error) {
	return f.members, f.membersErr
}

func (f *fakeControlPlane) CreateMigration(
	context.Context, string, string, string,
) (controlplane.Migration, error) {
	f.creates++
	if f.createErr != nil {
		return controlplane.Migration{}, f.createErr
	}
	return f.created, nil
}

func (f *fakeControlPlane) GetMigration(
	context.Context, string, string, string,
) (controlplane.Migration, error) {
	f.reads++
	return f.read, f.readErr
}

func (f *fakeControlPlane) ContinueMigration(context.Context, string, string, string) error {
	f.continues++
	return f.continueErr
}

func (f *fakeControlPlane) CancelMigration(context.Context, string, string, string) error {
	f.cancels++
	return f.cancelErr
}
