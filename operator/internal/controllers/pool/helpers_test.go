// The fixtures every test in this package builds on: a scheme, a fake client, a
// control plane that answers from a table, and an event sink that remembers what
// it was told.
//
// They live in one file because the alternative is each test file reconstructing
// the same four things slightly differently, and the differences are where a
// test stops testing what it says it does: a fake client without the status
// subresource silently accepts a status patch as a spec write, and a reconciler
// pointed at the real control-plane URL fails for a reason that has nothing to
// do with the test.

package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	testNamespace   = "simplyblock"
	testCluster     = "production"
	testClusterUUID = "4f2c8a11-6b3d-4e19-9a55-0c7e1d8f2b34"
	testPoolUUID    = "9b1d3c77-5a02-4f6e-8c31-2e7a4b905fd1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		scheme.AddToScheme,
		corev1.AddToScheme,
		storagev1.AddToScheme,
		simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the test scheme: %v", err)
		}
	}
	return s
}

// newClient returns a fake client with the status subresource enabled for both
// kinds this package writes status on. Without it a Status().Patch is applied to
// the whole object, so a test that means to assert a status write would pass on
// a reconciler that wrote the spec.
func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(
			&simplyblockv1alpha2.StoragePool{},
			&simplyblockv1alpha2.StoragePoolOps{},
		).
		WithObjects(objects...).
		Build()
}

// newCluster is the StorageCluster a pool names. Only the UUID varies between
// tests: an empty one is a cluster that is not finished, which is what a pool
// applied alongside its cluster waits on.
func newCluster(uuid string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: objectMeta(testCluster, testNamespace),
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: uuid},
	}
}

func newPool(name string, mutate ...func(*simplyblockv1alpha2.StoragePool)) *simplyblockv1alpha2.StoragePool {
	p := &simplyblockv1alpha2.StoragePool{
		ObjectMeta: objectMeta(name, testNamespace),
		Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: testCluster},
	}
	p.Finalizers = []string{FinalizerStoragePool}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func newClass(name string, labels map[string]string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  objectMetaWithLabels(name, "", labels),
		Provisioner: "csi.simplyblock.io",
		Parameters:  params,
	}
}

// controlPlane is a stub of the control plane's storage-pool endpoints. It
// answers from a table so a test states the backend's behavior rather than
// scripting a sequence of responses, and it counts the calls that are not
// idempotent, which is what the creation tests assert on.
type controlPlane struct {
	t *testing.T

	// pool is what a GET of the pool returns. An empty ID makes the GET a 404.
	pool poolDTO
	// list is what a GET of the collection returns, which is how the adoption
	// path finds a pool the control plane already has.
	list []poolDTO

	// createStatus and createBody override the response to a POST, so a test can
	// state a refusal without building one.
	createStatus int
	createBody   string
	// deleteStatus overrides the response to a DELETE.
	deleteStatus int

	// onCreate runs at the moment a POST arrives, before it is answered. It is
	// how a test observes what Kubernetes held while the call was in flight.
	onCreate func()

	posts   int
	deletes int
	hosts   []string

	server *httptest.Server
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	cp := &controlPlane{t: t, pool: poolDTO{ID: testPoolUUID, Status: "online"}}
	cp.server = httptest.NewServer(http.HandlerFunc(cp.handle))
	t.Cleanup(cp.server.Close)
	return cp
}

// client returns a webapi client pointed at the stub, in the form the reconciler
// takes it.
func (cp *controlPlane) client() func() *webapi.Client {
	return func() *webapi.Client {
		return &webapi.Client{BaseURL: cp.server.URL, HttpClient: cp.server.Client()}
	}
}

func (cp *controlPlane) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && hasSuffix(r.URL.Path, "/host"):
		cp.hosts = append(cp.hosts, "added")
		writeJSON(w, map[string]string{})
	case r.Method == http.MethodDelete && hasSuffix(r.URL.Path, "/host"):
		cp.hosts = append(cp.hosts, "removed")
		writeJSON(w, map[string]string{})
	case r.Method == http.MethodPost:
		cp.posts++
		if cp.onCreate != nil {
			cp.onCreate()
		}
		if cp.createStatus != 0 {
			w.WriteHeader(cp.createStatus)
			_, _ = w.Write([]byte(cp.createBody))
			return
		}
		writeJSON(w, cp.pool)
	case r.Method == http.MethodDelete:
		cp.deletes++
		status := cp.deleteStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	case r.Method == http.MethodGet && hasSuffix(r.URL.Path, "/storage-pools/"):
		writeJSON(w, cp.list)
	case r.Method == http.MethodGet:
		if cp.pool.ID == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, cp.pool)
	default:
		cp.t.Fatalf("the control plane stub was asked for %s %s", r.Method, r.URL.Path)
	}
}

// writeJSON answers a request that succeeded. Every refusal this stub can make
// is driven by a field on the struct, so the status is never anything else here.
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// recordedEvent is one event, flattened to what a test asserts on.
type recordedEvent struct {
	Type   string
	Reason string
}

// recorder remembers the events it is told about. events.EventRecorder has one
// method that matters here, and a fake is less machinery than a real broadcaster
// with a fake clientset behind it.
type recorder struct {
	events []recordedEvent
}

func (r *recorder) Eventf(
	_ runtime.Object, _ runtime.Object, eventType, reason, _, _ string, _ ...any,
) {
	r.events = append(r.events, recordedEvent{Type: eventType, Reason: reason})
}

// count returns how many events carried a reason, which is what a test asserting
// "once, not once per pass" needs.
func (r *recorder) count(reason string) int {
	n := 0
	for _, e := range r.events {
		if e.Reason == reason {
			n++
		}
	}
	return n
}

func (r *recorder) has(reason string) bool { return r.count(reason) > 0 }

var _ events.EventRecorder = (*recorder)(nil)

func objectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace}
}

func objectMetaWithLabels(name, namespace string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}
}

// assignmentLabels are the three labels a class assigned to the test pool
// carries, with an optional fourth for a class the operator wrote.
func assignmentLabels(poolName string, managed bool) map[string]string {
	labels := map[string]string{
		LabelNamespace: testNamespace,
		LabelCluster:   testCluster,
		LabelPool:      poolName,
	}
	if managed {
		labels[LabelManagedBy] = ManagedByStorageCluster
	}
	return labels
}
