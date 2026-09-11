// Tests for the StoragePool admission guard.
//
// The cases are design-storagepool.md §3.4, and the line they draw is which
// condition can ever become true. A cluster that does not exist is refused,
// because spec.clusterRef is immutable and the object could never be corrected.
// A cluster that exists and is not finished is admitted, because a manifest that
// declares a cluster and its pools in one apply is the ordinary way to bring a
// deployment up, and refusing it would make that apply fail on ordering.

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func newPoolScheme(t *testing.T) *runtime.Scheme {
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

func poolRaw(t *testing.T, p *simplyblockv1alpha2.StoragePool) runtime.RawExtension {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal StoragePool: %v", err)
	}
	return runtime.RawExtension{Raw: b}
}

// The cluster every pool below names, and the pool that names it. Only the
// namespace and the UUID vary: what the cases differ in is where the cluster is
// and whether it is finished, not what either object is called.
const (
	testValidatorCluster = "production"
	testValidatorPool    = "tenant-a"
)

func testStorageCluster(namespace, uuid string) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorCluster, Namespace: namespace},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: uuid},
	}
}

func testStoragePool(namespace string) *simplyblockv1alpha2.StoragePool {
	return &simplyblockv1alpha2.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorPool, Namespace: namespace},
		Spec:       simplyblockv1alpha2.StoragePoolSpec{ClusterRef: testValidatorCluster},
	}
}

func TestStoragePoolValidator(t *testing.T) {
	tests := []struct {
		name      string
		existing  []client.Object
		operation admissionv1.Operation
		pool      *simplyblockv1alpha2.StoragePool
		allowed   bool
		message   string
	}{
		{
			name:      "a cluster that exists and is finished",
			existing:  []client.Object{testStorageCluster("simplyblock", "a-uuid")},
			operation: admissionv1.Create,
			pool:      testStoragePool("simplyblock"),
			allowed:   true,
		},
		{
			name: "a cluster that exists and is not finished, which is a not-yet",
			existing: []client.Object{
				testStorageCluster("simplyblock", ""),
			},
			operation: admissionv1.Create,
			pool:      testStoragePool("simplyblock"),
			allowed:   true,
		},
		{
			name:      "a cluster that does not exist",
			existing:  nil,
			operation: admissionv1.Create,
			pool:      testStoragePool("simplyblock"),
			allowed:   false,
			message:   "production",
		},
		{
			name:      "a cluster of that name in another namespace",
			existing:  []client.Object{testStorageCluster("elsewhere", "a-uuid")},
			operation: admissionv1.Create,
			pool:      testStoragePool("simplyblock"),
			allowed:   false,
			message:   "simplyblock",
		},
		{
			// An update cannot introduce a dangling reference, because
			// spec.clusterRef is immutable, and refusing one would refuse an edit
			// to a pool whose cluster has since been deleted — which is exactly
			// the object somebody is trying to clean up.
			name:      "an update is not the validator's business",
			existing:  nil,
			operation: admissionv1.Update,
			pool:      testStoragePool("simplyblock"),
			allowed:   true,
		},
		{
			name:      "a delete is not the validator's business",
			existing:  nil,
			operation: admissionv1.Delete,
			pool:      testStoragePool("simplyblock"),
			allowed:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newPoolScheme(t)
			validator := &StoragePoolValidator{
				Client:  fake.NewClientBuilder().WithScheme(s).WithObjects(tc.existing...).Build(),
				Decoder: admission.NewDecoder(s),
			}

			response := validator.Handle(context.Background(), admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: tc.operation,
					Namespace: tc.pool.Namespace,
					Object:    poolRaw(t, tc.pool),
				},
			})

			if response.Allowed != tc.allowed {
				t.Fatalf("allowed = %t, want %t (%s)",
					response.Allowed, tc.allowed, response.Result.Message)
			}
			if tc.message != "" && !strings.Contains(response.Result.Message, tc.message) {
				t.Errorf("the refusal %q does not mention %q", response.Result.Message, tc.message)
			}
		})
	}
}

// A namespaced object created through a namespaced endpoint may arrive with the
// field unset, because the path carries it instead. The validator has to look in
// the right namespace either way.
func TestStoragePoolValidatorFallsBackToTheRequestNamespace(t *testing.T) {
	s := newPoolScheme(t)
	validator := &StoragePoolValidator{
		Client: fake.NewClientBuilder().WithScheme(s).
			WithObjects(testStorageCluster("simplyblock", "a-uuid")).Build(),
		Decoder: admission.NewDecoder(s),
	}

	pool := testStoragePool("")
	response := validator.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: "simplyblock",
			Object:    poolRaw(t, pool),
		},
	})

	if !response.Allowed {
		t.Errorf("the pool was refused: %s", response.Result.Message)
	}
}
