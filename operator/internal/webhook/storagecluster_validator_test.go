// Tests for the StorageCluster admission guard.
//
// The rule under test is one cluster per namespace, and the cases are the two
// sides of why it exists. A cluster renders a workload whose serving certificate
// is named for atlas-lib/kube.StorageNodeSetAPIServiceName, which is a constant,
// so the second cluster in a namespace cannot own it: its workload pass aborts
// at that step and never reaches the DaemonSet four steps later. What the
// operator then shows is a cluster whose nodes wait forever on a hostname with
// no pod behind it, which is why this is refused at admission instead.

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func newClusterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		simplyblockv1alpha1.AddToScheme,
		simplyblockv1alpha2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build the test scheme: %v", err)
		}
	}
	return s
}

func clusterAt(namespace, name string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func clusterRequest(
	t *testing.T, op admissionv1.Operation, cluster *simplyblockv1alpha2.StorageCluster,
) admission.Request {
	t.Helper()
	encoded, err := json.Marshal(cluster)
	if err != nil {
		t.Fatalf("encode the cluster: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op,
		Namespace: cluster.Namespace,
		Name:      cluster.Name,
		Object:    runtime.RawExtension{Raw: encoded},
	}}
}

func TestTheFirstClusterOfANamespaceIsAdmitted(t *testing.T) {
	scheme := newClusterScheme(t)
	validator := &StorageClusterValidator{
		Client:  fake.NewClientBuilder().WithScheme(scheme).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, clusterAt("cluster1", "first")))

	if !response.Allowed {
		t.Fatalf("the only cluster of a namespace was refused: %s",
			response.Result.Message)
	}
}

func TestASecondClusterInOneNamespaceIsRefused(t *testing.T) {
	scheme := newClusterScheme(t)
	validator := &StorageClusterValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(clusterAt("cluster1", "already-there")).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, clusterAt("cluster1", "second")))

	if response.Allowed {
		t.Fatal("a second cluster in one namespace was admitted")
	}
	// The refusal has to name the cluster already there and say what to do,
	// because the person reading it is holding a manifest that looks correct.
	message := response.Result.Message
	for _, want := range []string{"already-there", "namespace"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not mention %q: %s", want, message)
		}
	}
}

func TestAClusterIsAdmittedBesideOneInAnotherNamespace(t *testing.T) {
	scheme := newClusterScheme(t)
	validator := &StorageClusterValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(clusterAt("cluster1", "elsewhere")).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, clusterAt("cluster2", "mine")))

	if !response.Allowed {
		t.Fatalf("a cluster was refused for one in another namespace: %s",
			response.Result.Message)
	}
}

// An update carries the cluster that is already there, so counting namespace
// members without excluding the object under review refuses every edit a
// deployed cluster ever receives.
func TestUpdatingTheOnlyClusterIsAdmitted(t *testing.T) {
	scheme := newClusterScheme(t)
	existing := clusterAt("cluster1", "only")
	validator := &StorageClusterValidator{
		Client:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Update, existing))

	if !response.Allowed {
		t.Fatalf("an update to the only cluster of a namespace was refused: %s",
			response.Result.Message)
	}
}
