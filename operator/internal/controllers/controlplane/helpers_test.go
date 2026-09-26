// The fixtures and the fake control plane the tests in this package are built
// on.
//
// The scheme is built once and carries every kind an install writes, because an
// apply resolves a typed object's kind through it and a missing registration
// fails at a place that says nothing about which object was missing.

package controlplane

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	testNamespace = "simplyblock"
	testImage     = "quay.io/simplyblock-io/simplyblock:26.2.8"
)

// testScheme carries every kind the install writes, plus the FoundationDB kinds
// as unstructured so a fake client can hold one.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("build the scheme: %v", err)
		}
	}
	// The FoundationDB kinds have no Go type in this repository, so the fake
	// client is told their list kind by hand. Without it a Get of one fails with
	// a missing registration, which reads as a bug in the code under test.
	scheme.AddKnownTypeWithName(fdbClusterGVK, &unstructuredStub{})
	scheme.AddKnownTypeWithName(fdbClusterGVK.GroupVersion().WithKind("FoundationDBClusterList"),
		&unstructuredListStub{})
	scheme.AddKnownTypeWithName(fdbBackupGVK, &unstructuredStub{})
	scheme.AddKnownTypeWithName(fdbBackupGVK.GroupVersion().WithKind("FoundationDBBackupList"),
		&unstructuredListStub{})
	return scheme
}

// managedControlPlane is the fixture every managed test starts from: the
// singleton, in the namespace, naming an image.
func localControlPlane() *simplyblockv1alpha2.ControlPlane {
	return &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: testNamespace},
		Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{
				Local: &simplyblockv1alpha2.LocalControlPlane{Image: testImage},
			},
		},
	}
}

// managedControlPlane names a control plane that already exists.
func managedControlPlane(endpoint string) *simplyblockv1alpha2.ControlPlane {
	return &simplyblockv1alpha2.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: testNamespace},
		Spec: simplyblockv1alpha2.ControlPlaneSpec{
			Source: simplyblockv1alpha2.ControlPlaneSource{
				Managed: &simplyblockv1alpha2.ManagedControlPlane{
					Endpoint:             endpoint,
					CredentialsSecretRef: &corev1.LocalObjectReference{Name: "cp-token"},
				},
			},
		},
	}
}

// stubProber answers the two reads without an HTTP server, which is what lets
// the phase branches be exercised at all: every one of them turns on what the
// control plane said.
type stubProber struct {
	ready        bool
	readyMessage string
	version      string
	versionErr   error

	// readyCalls and versionCalls count what the reconciler asked for, which is
	// how a test asserts that a held step did not probe.
	readyCalls   int
	versionCalls int
}

func (p *stubProber) Ready(context.Context, string) (bool, string) {
	p.readyCalls++
	return p.ready, p.readyMessage
}

func (p *stubProber) Version(context.Context, string) (string, error) {
	p.versionCalls++
	return p.version, p.versionErr
}

// newClient builds a fake client holding the given objects, with the status
// subresource enabled for the two kinds this package writes one on.
func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(
			&simplyblockv1alpha2.ControlPlane{},
			&simplyblockv1alpha2.ControlPlaneOps{},
		).
		Build()
}

// deployment is a running workload with a ready count, which is what the
// component table reads.
func deployment(name string, desired, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(desired)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func statefulSet(name string, desired, ready int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(desired)},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: ready},
	}
}

// componentStatus is one row of what observe produces, built directly so a phase
// test does not have to stand up the workloads behind it.
func componentStatus(name string, desired, ready int32, essential bool) simplyblockv1alpha2.ControlPlaneComponentStatus {
	return simplyblockv1alpha2.ControlPlaneComponentStatus{
		Name: name, Desired: desired, Ready: ready, Essential: essential,
	}
}

// findObject looks up one built object by kind and name, which is how the
// workload tests assert that a step writes what it says it writes.
func findObject(objects []client.Object, name string) client.Object {
	for _, obj := range objects {
		if obj.GetName() == name {
			return obj
		}
	}
	return nil
}

// findRole, findDeployment, and the rest narrow that to the type a test asserts
// against, failing rather than returning nil so the assertion that follows is
// about the object rather than about a nil pointer.
func findDeployment(t *testing.T, objects []client.Object, name string) *appsv1.Deployment {
	t.Helper()
	obj := findObject(objects, name)
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("no Deployment named %q among the built objects", name)
	}
	return d
}

// findEnvVar locates a container env entry by name in a Deployment's first
// container, failing rather than returning a zero value so a missing entry
// reads as the assertion it is instead of a nil-field panic later.
func findEnvVar(t *testing.T, d *appsv1.Deployment, name string) corev1.EnvVar {
	t.Helper()
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no %q env var on %s", name, d.Name)
	return corev1.EnvVar{}
}

func findClusterRole(t *testing.T, objects []client.Object, name string) *rbacv1.ClusterRole {
	t.Helper()
	obj := findObject(objects, name)
	role, ok := obj.(*rbacv1.ClusterRole)
	if !ok {
		t.Fatalf("no ClusterRole named %q among the built objects", name)
	}
	return role
}

// unstructuredStub and unstructuredListStub let the scheme name the FoundationDB
// kinds. The fake client stores and returns unstructured objects for them; these
// exist only so the kind resolves.
type unstructuredStub struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}

func (in *unstructuredStub) DeepCopyObject() runtime.Object {
	out := *in
	return &out
}

type unstructuredListStub struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []unstructuredStub `json:"items"`
}

func (in *unstructuredListStub) DeepCopyObject() runtime.Object {
	out := *in
	out.Items = append([]unstructuredStub(nil), in.Items...)
	return &out
}

// recordingRecorder remembers the events it is told about. events.EventRecorder
// has one method that matters here, and a fake is less machinery than a real
// broadcaster with a fake clientset behind it.
type recordingRecorder struct {
	reasons []string
	types   []string
}

func (r *recordingRecorder) Eventf(
	_ runtime.Object, _ runtime.Object, eventType, reason, _, _ string, _ ...any,
) {
	r.types = append(r.types, eventType)
	r.reasons = append(r.reasons, reason)
}

// count returns how many events carried a reason, which is what a test
// asserting "once, not once per pass" needs.
func (r *recordingRecorder) count(reason string) int {
	n := 0
	for _, got := range r.reasons {
		if got == reason {
			n++
		}
	}
	return n
}

var _ events.EventRecorder = (*recordingRecorder)(nil)
