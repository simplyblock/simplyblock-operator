// A run naming NetworkAttachmentDefinitions is checked against the namespace
// before a single worker is probed: a definition that does not exist fails the
// run at once and loudly, and a cluster without the kind runs on, to be drafted
// on host networking.

package deployment

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

var testNADKind = schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"}

// namedNAD is a definition in the run's namespace.
func namedNAD(name string) *unstructured.Unstructured {
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(testNADKind)
	nad.SetNamespace(opsNamespace)
	nad.SetName(name)
	return nad
}

// attachedRunner is a runner on a cluster that serves the kind, unless served
// is false, with the definitions given and a recorder for the events. The run
// names storage-mgmt for management.
func attachedRunner(
	t *testing.T, served bool, nads ...*unstructured.Unstructured,
) (*runner, *events.FakeRecorder) {
	t.Helper()
	scheme := opsScheme(t)
	scheme.AddKnownTypeWithName(testNADKind, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(testNADKind.GroupVersion().WithKind(testNADKind.Kind+"List"), &unstructured.UnstructuredList{})

	objects := make([]client.Object, 0, 3+len(nads))
	objects = append(objects,
		discoverRun(&simplyblockv1alpha2.DiscoverSpec{
			NetworkAttachment: &simplyblockv1alpha2.NetworkAttachmentSelection{Management: "storage-mgmt"},
		}),
		worker("worker-1"), worker("worker-2"))
	for _, nad := range nads {
		objects = append(objects, nad)
	}
	funcs := interceptor.Funcs{}
	if !served {
		funcs.List = func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Group == testNADKind.Group {
				return &meta.NoKindMatchError{GroupKind: testNADKind.GroupKind(), SearchedVersions: []string{"v1"}}
			}
			return c.List(ctx, list, opts...)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&simplyblockv1alpha2.OperatorOps{}).WithInterceptorFuncs(funcs).Build()
	recorder := events.NewFakeRecorder(64)
	return &runner{t: t, client: c, reconciler: &OperatorOpsReconciler{
		Client: c, Scheme: scheme, ProbeImage: opsImage, Recorder: recorder,
	}}, recorder
}

// warnings is every Warning event recorded.
func warnings(recorder *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case line := <-recorder.Events:
			if strings.HasPrefix(line, "Warning") {
				out = append(out, line)
			}
		default:
			return out
		}
	}
}

func TestARunNamingADefinitionTheNamespaceDoesNotHoldFailsBeforeProbing(t *testing.T) {
	r, recorder := attachedRunner(t, true, namedNAD("storage-data"), namedNAD("storage-other"))

	r.step()
	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseFailed {
		t.Fatalf("the run is %q on step %q, want Failed", ops.Status.Phase, ops.Status.Step.State)
	}
	if !strings.Contains(ops.Status.Message, "storage-mgmt") ||
		!strings.Contains(ops.Status.Message, "storage-data, storage-other") {
		t.Errorf("the message %q does not name the missing definition and the ones there are", ops.Status.Message)
	}
	if jobs := r.jobs(); len(jobs) != 0 {
		t.Errorf("%d probe Jobs were created for a run that cannot be drafted", len(jobs))
	}
	if found := warnings(recorder); len(found) == 0 || !strings.Contains(strings.Join(found, "\n"), "storage-mgmt") {
		t.Errorf("no Warning event names the missing definition: %v", found)
	}
}

// dev-ocp: the kind is served and the namespace holds no definition at all.
func TestARunNamingADefinitionInANamespaceWithNoneFailsBeforeProbing(t *testing.T) {
	r, _ := attachedRunner(t, true)

	r.step()
	_, ops := r.step()

	if ops.Status.Phase != simplyblockv1alpha2.OperatorOpsPhaseFailed {
		t.Fatalf("the run is %q on step %q, want Failed", ops.Status.Phase, ops.Status.Step.State)
	}
	if !strings.Contains(ops.Status.Message, "storage-mgmt") || !strings.Contains(ops.Status.Message, "holds none") {
		t.Errorf("the message %q does not say the namespace holds no definitions", ops.Status.Message)
	}
}

func TestARunNamingADefinitionTheNamespaceHoldsProbes(t *testing.T) {
	r, _ := attachedRunner(t, true, namedNAD("storage-mgmt"))

	r.step()
	_, ops := r.step()

	if ops.Status.Step.State != string(simplyblockv1alpha2.OperatorOpsStepProbing) {
		t.Errorf("the run is %q on step %q (%s), want Probing", ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
}

// Without Multus the run goes on, and the draft keeps host networking.
func TestARunNamingADefinitionOnAClusterWithoutTheKindProbes(t *testing.T) {
	r, _ := attachedRunner(t, false)

	r.step()
	_, ops := r.step()

	if ops.Status.Step.State != string(simplyblockv1alpha2.OperatorOpsStepProbing) {
		t.Errorf("the run is %q on step %q (%s), want Probing", ops.Status.Phase, ops.Status.Step.State, ops.Status.Message)
	}
}
