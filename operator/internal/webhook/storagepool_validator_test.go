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

	"github.com/simplyblock/atlas/ptr"

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

func testStorageCluster(namespace, uuid string) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorCluster, Namespace: namespace},
		Status:     simplyblockv1alpha2.StorageClusterStatus{UUID: uuid},
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

// U-196 is superseded: a pool on a supercluster is one backend pool per member
// (design-superclusters.md §7.1), so a pool naming a supercluster is admitted
// like one naming a cluster.
func TestAPoolOnASuperclusterIsAdmitted(t *testing.T) {
	scheme := newPoolScheme(t)
	supercluster := testStorageCluster("sb", "0d6f1b2e-1f0a-4c71-9a0e-5b7d2c3e4f50")
	supercluster.Spec.Supercluster = &simplyblockv1alpha2.SuperclusterSpec{}
	v := &StoragePoolValidator{
		Client:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(supercluster).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	resp := v.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: "sb",
		Object:    poolRaw(t, testStoragePool("sb")),
	}})

	if !resp.Allowed {
		t.Fatalf("a pool naming a supercluster was refused: %s", resp.Result.Message)
	}
}

// storageNodesOf is n storage nodes of cluster, the first `removing` of them on
// their way out.
func storageNodesOf(cluster string, n, removing int) []client.Object {
	objs := make([]client.Object, 0, n)
	for i := range n {
		node := &simplyblockv1alpha2.StorageNode{
			ObjectMeta: metav1.ObjectMeta{Name: cluster + "-node-" + string(rune('a'+i)), Namespace: "simplyblock"},
			Spec:       simplyblockv1alpha2.StorageNodeSpec{ClusterRef: cluster},
		}
		if i < removing {
			node.Status.Phase = simplyblockv1alpha2.StorageNodePhaseRemoving
		}
		objs = append(objs, node)
	}
	return objs
}

// stripedPool is a pool whose volumes are striped width wide.
func stripedPool(width int32) *simplyblockv1alpha2.StoragePool {
	pool := testStoragePool("simplyblock")
	pool.Spec.VolumeDefaults = &simplyblockv1alpha2.VolumeDefaults{StripeWidth: &width}
	return pool
}

// A striped pool is refused when its cluster cannot serve one of its volumes:
// one with fewer storage nodes than members, or a stretched one. What the
// cluster does not know yet, it does not hold against the pool.
func TestStoragePoolValidatorChecksTheStripeAgainstTheCluster(t *testing.T) {
	stretched := testStorageCluster("simplyblock", "a-uuid")
	stretched.Spec.EnableSyncReplication = ptr.To(true)

	supercluster := testStorageCluster("simplyblock", "a-uuid")
	supercluster.Spec.Supercluster = &simplyblockv1alpha2.SuperclusterSpec{}
	supercluster.Status.Members = []simplyblockv1alpha2.SuperclusterMemberStatus{
		{Name: "member-big", PlacementEligible: true},
		{Name: "member-small", PlacementEligible: true},
		{Name: "member-draining", PlacementEligible: false},
	}

	withNodes := func(cluster client.Object, nodes ...[]client.Object) []client.Object {
		objs := []client.Object{cluster}
		for _, n := range nodes {
			objs = append(objs, n...)
		}
		return objs
	}

	tests := []struct {
		name     string
		existing []client.Object
		pool     *simplyblockv1alpha2.StoragePool
		allowed  bool
		message  string
	}{
		{
			name:     "a width the cluster's nodes cover",
			existing: withNodes(testStorageCluster("simplyblock", "a-uuid"), storageNodesOf(testValidatorCluster, 3, 0)),
			pool:     stripedPool(3),
			allowed:  true,
		},
		{
			name:     "wider than the cluster",
			existing: withNodes(testStorageCluster("simplyblock", "a-uuid"), storageNodesOf(testValidatorCluster, 3, 0)),
			pool:     stripedPool(4),
			message:  "has 3 storage nodes",
		},
		{
			name:     "a node on its way out is not counted",
			existing: withNodes(testStorageCluster("simplyblock", "a-uuid"), storageNodesOf(testValidatorCluster, 3, 1)),
			pool:     stripedPool(3),
			message:  "has 2 storage nodes",
		},
		{
			name:     "a cluster with no nodes yet says nothing",
			existing: withNodes(testStorageCluster("simplyblock", "")),
			pool:     stripedPool(8),
			allowed:  true,
		},
		{
			name:     "a width of 1 is not striped and is not checked",
			existing: withNodes(stretched),
			pool:     stripedPool(1),
			allowed:  true,
		},
		{
			name:     "a stretched cluster",
			existing: withNodes(stretched, storageNodesOf(testValidatorCluster, 4, 0)),
			pool:     stripedPool(2),
			message:  "synchronously replicated",
		},
		{
			name: "a supercluster member that may take volumes is too small",
			existing: withNodes(supercluster, storageNodesOf("member-big", 4, 0),
				storageNodesOf("member-small", 2, 0), storageNodesOf("member-draining", 1, 0)),
			pool:    stripedPool(3),
			message: "member-small",
		},
		{
			name: "a member that takes no new volumes is not held against the pool",
			existing: withNodes(supercluster, storageNodesOf("member-big", 4, 0),
				storageNodesOf("member-small", 3, 0), storageNodesOf("member-draining", 1, 0)),
			pool:    stripedPool(3),
			allowed: true,
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
					Operation: admissionv1.Create,
					Namespace: tc.pool.Namespace,
					Object:    poolRaw(t, tc.pool),
				},
			})
			if response.Allowed != tc.allowed {
				t.Fatalf("allowed = %t, want %t (%s)", response.Allowed, tc.allowed, response.Result.Message)
			}
			if tc.message != "" && !strings.Contains(response.Result.Message, tc.message) {
				t.Errorf("the refusal %q does not mention %q", response.Result.Message, tc.message)
			}
		})
	}
}
