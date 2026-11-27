// Tests for the StorageCluster admission guard's namespace rule.
//
// Every cluster records workload names of its own (design-superclusters.md
// §6.1), and a supercluster's members share the Services the supercluster
// builds (§6.2), so any number of clusters of any role share a namespace.

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
	"k8s.io/utils/ptr"
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

func TestASecondClusterInOneNamespaceIsAdmitted(t *testing.T) {
	scheme := newClusterScheme(t)
	validator := &StorageClusterValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(clusterAt("cluster1", "already-there")).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, clusterAt("cluster1", "second")))

	if !response.Allowed {
		t.Fatalf("a second cluster in one namespace was refused: %s", response.Result.Message)
	}
}

func memberOf(namespace, name, supercluster string) *simplyblockv1alpha2.StorageCluster {
	member := clusterAt(namespace, name)
	member.Spec.SuperclusterRef = supercluster
	return member
}

func superclusterNamed(namespace, name string) *simplyblockv1alpha2.StorageCluster {
	supercluster := clusterAt(namespace, name)
	supercluster.Spec.Supercluster = &simplyblockv1alpha2.SuperclusterSpec{}
	return supercluster
}

func TestAMemberBesideAnotherClusterIsAdmitted(t *testing.T) {
	scheme := newClusterScheme(t)
	validator := &StorageClusterValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			clusterAt("cluster1", "already-there"), superclusterNamed("cluster1", "prod"),
			memberOf("cluster1", "prod-dense", "prod"),
		).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, memberOf("cluster1", "prod-fast", "prod")))

	if !response.Allowed {
		t.Fatalf("a member was refused beside other clusters: %s", response.Result.Message)
	}
}

func TestAStandaloneClusterBesideAMemberIsAdmitted(t *testing.T) {
	scheme := newClusterScheme(t)
	validator := &StorageClusterValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			superclusterNamed("cluster1", "prod"), memberOf("cluster1", "prod-fast", "prod"),
		).Build(),
		Decoder: admission.NewDecoder(scheme),
	}

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, clusterAt("cluster1", "standalone")))

	if !response.Allowed {
		t.Fatalf("a standalone cluster was refused beside a member: %s", response.Result.Message)
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

// holdMs and leaseTtlMs are admitted, so manifests that set them still apply,
// and the admission says they have no effect. An arbitration block without them
// draws no warning.
//
// Regression: 2026-11-26-two-node-timings-never-sent — the operator accepted
// and validated both fields and sent neither, and the control plane holds for
// a fixed 2500 ms and grants 1500 ms leases, so an administrator tuning them
// believed the cluster ran at timings it never did.
func TestTwoNodeTimingsAreAdmittedWithAWarning(t *testing.T) {
	scheme := newClusterScheme(t)
	validator := &StorageClusterValidator{
		Client:  fake.NewClientBuilder().WithScheme(scheme).Build(),
		Decoder: admission.NewDecoder(scheme),
	}
	dual := func(arbitration *simplyblockv1alpha2.ArbitrationSpec) *simplyblockv1alpha2.StorageCluster {
		cluster := clusterAt("edge", "pair")
		cluster.Spec.ClusterMode = simplyblockv1alpha2.ClusterModeDual
		cluster.Spec.EnableArbitration = ptr.To(true)
		cluster.Spec.Arbitration = arbitration
		return cluster
	}

	for _, op := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
		response := validator.Handle(context.Background(), clusterRequest(t, op, dual(
			&simplyblockv1alpha2.ArbitrationSpec{HoldMs: ptr.To(int32(3000)), LeaseTtlMs: ptr.To(int32(2000))})))
		if !response.Allowed {
			t.Fatalf("%s: a cluster setting holdMs was refused: %s", op, response.Result.Message)
		}
		warnings := strings.Join(response.Warnings, "\n")
		for _, field := range []string{"holdMs", "leaseTtlMs", "2500", "1500"} {
			if !strings.Contains(warnings, field) {
				t.Errorf("%s: the warnings do not mention %s: %q", op, field, response.Warnings)
			}
		}
	}

	response := validator.Handle(context.Background(), clusterRequest(t, admissionv1.Create,
		dual(&simplyblockv1alpha2.ArbitrationSpec{HostFencing: ptr.To(true)})))
	if len(response.Warnings) != 0 {
		t.Errorf("an arbitration block without timings drew warnings: %q", response.Warnings)
	}
}
