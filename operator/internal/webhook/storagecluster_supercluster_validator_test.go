// Tests for the supercluster rules of the StorageCluster admission guard
// (design-superclusters.md §4.2, §4.4).
//
// Membership is declared by the member, in an immutable field, so a member is
// refused when the supercluster it names is not there to be named. A
// supercluster has no nodes, so it may carry only what describes the aggregate,
// and a spec field the API server filled from its schema default is not a field
// its author wrote. The one-cluster-per-namespace rule still holds for clusters
// that carry a storage-node workload, and a supercluster carries none.

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/crds"
)

func superclusterAt(namespace, name string) *simplyblockv1alpha2.StorageCluster {
	c := clusterAt(namespace, name)
	c.Spec.Supercluster = &simplyblockv1alpha2.SuperclusterSpec{}
	return c
}

func memberAt(namespace, name, supercluster string) *simplyblockv1alpha2.StorageCluster {
	c := clusterAt(namespace, name)
	c.Spec.SuperclusterRef = supercluster
	c.Spec.MaxSubsystemCount = ptr.To(int32(20))
	c.Spec.VCPUCount = ptr.To(int32(8))
	return c
}

func clusterValidator(t *testing.T, objects ...client.Object) *StorageClusterValidator {
	t.Helper()
	scheme := newClusterScheme(t)
	return &StorageClusterValidator{
		Client:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Decoder: admission.NewDecoder(scheme),
	}
}

// rawClusterRequest sends a cluster whose spec is written as JSON, which is
// what the API server sends after defaulting: a field the Go type renders only
// when it is set would otherwise be impossible to send at its default.
func rawClusterRequest(t *testing.T, namespace, name string, spec map[string]any) admission.Request {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"apiVersion": "storage.simplyblock.io/v1alpha2",
		"kind":       "StorageCluster",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       spec,
	})
	if err != nil {
		t.Fatalf("encode the cluster: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: namespace,
		Name:      name,
		Object:    runtime.RawExtension{Raw: encoded},
	}}
}

func deleteClusterRequest(t *testing.T, cluster *simplyblockv1alpha2.StorageCluster) admission.Request {
	t.Helper()
	encoded, err := json.Marshal(cluster)
	if err != nil {
		t.Fatalf("encode the cluster: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		Namespace: cluster.Namespace,
		Name:      cluster.Name,
		OldObject: runtime.RawExtension{Raw: encoded},
	}}
}

func wantAllowed(t *testing.T, response admission.Response, what string) {
	t.Helper()
	if !response.Allowed {
		t.Fatalf("%s was refused: %s", what, response.Result.Message)
	}
}

func wantDenied(t *testing.T, response admission.Response, what string, mentions ...string) {
	t.Helper()
	if response.Allowed {
		t.Fatalf("%s was admitted", what)
	}
	for _, want := range mentions {
		if !strings.Contains(response.Result.Message, want) {
			t.Errorf("the refusal of %s does not mention %q: %s", what, want, response.Result.Message)
		}
	}
}

// U-139
func TestAMemberNamingASuperclusterInItsNamespaceIsAdmitted(t *testing.T) {
	validator := clusterValidator(t, superclusterAt("sb", "prod"))

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, memberAt("sb", "prod-dense-kqv", "prod")))

	wantAllowed(t, response, "a member of an existing supercluster")
}

// U-140, U-146: the reference is immutable, so a member whose supercluster
// cannot be found could never be corrected.
func TestAMemberWhoseSuperclusterDoesNotResolveIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing []client.Object
	}{
		{name: "nothing by that name, as when the member is created first"},
		{name: "a standalone cluster", existing: []client.Object{clusterAt("sb", "prod")}},
		{name: "a supercluster in another namespace", existing: []client.Object{superclusterAt("elsewhere", "prod")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validator := clusterValidator(t, tc.existing...)

			response := validator.Handle(context.Background(),
				clusterRequest(t, admissionv1.Create, memberAt("sb", "prod-dense-kqv", "prod")))

			wantDenied(t, response, "a member of "+tc.name, "prod-dense-kqv", "prod")
		})
	}
}

// U-142: membership is the member's statement, so a supercluster is admitted
// before any member names it.
func TestASuperclusterWithNoMembersIsAdmitted(t *testing.T) {
	validator := clusterValidator(t)

	response := validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, superclusterAt("sb", "prod")))

	wantAllowed(t, response, "a supercluster with no members")
}

// A supercluster carries no storage-node workload, so it shares a namespace
// with the one cluster there that does: its first member, or a standalone
// cluster.
func TestASuperclusterSharesANamespaceWithAWorkloadCluster(t *testing.T) {
	validator := clusterValidator(t, clusterAt("sb", "standalone"))
	wantAllowed(t, validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, superclusterAt("sb", "prod"))),
		"a supercluster beside a standalone cluster")

	validator = clusterValidator(t, superclusterAt("sb", "prod"))
	wantAllowed(t, validator.Handle(context.Background(),
		clusterRequest(t, admissionv1.Create, memberAt("sb", "prod-dense-kqv", "prod"))),
		"the first member beside its supercluster")
}

// U-36
func TestASuperclusterCarryingSizingIsRefusedNamingTheField(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
	}{
		{"vcpuCount", 8},
		{"stripe", map[string]any{"dataChunks": 2, "parityChunks": 1}},
		{"storageNodes", map[string]any{"mgmtInterface": "eth0"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			validator := clusterValidator(t)

			response := validator.Handle(context.Background(), rawClusterRequest(t, "sb", "prod",
				map[string]any{"supercluster": map[string]any{}, tc.field: tc.value}))

			wantDenied(t, response, "a supercluster carrying "+tc.field, "spec."+tc.field)
		})
	}
}

// U-181: the API server writes every schema default before admission, so a
// supercluster nobody gave a port arrives with one. The defaults are read
// from the CRD schema this binary carries, so the test builds the request
// from the same schema rather than from a list that could drift with it.
func TestASuperclusterAtItsSchemaDefaultsIsAdmitted(t *testing.T) {
	defaults := storageClusterSpecDefaultsForTest(t)
	if len(defaults) == 0 {
		t.Fatal("the embedded StorageCluster schema declares no spec defaults to test with")
	}
	spec := map[string]any{
		"supercluster":      map[string]any{},
		"warningThreshold":  map[string]any{"capacity": 80},
		"criticalThreshold": map[string]any{"capacity": 90},
	}
	for field, value := range defaults {
		spec[field] = value
	}
	validator := clusterValidator(t)

	response := validator.Handle(context.Background(), rawClusterRequest(t, "sb", "prod", spec))

	wantAllowed(t, response, "a supercluster carrying only schema defaults")
}

// U-182
func TestASuperclusterWithAPortOffItsDefaultIsRefused(t *testing.T) {
	validator := clusterValidator(t)

	response := validator.Handle(context.Background(), rawClusterRequest(t, "sb", "prod",
		map[string]any{"supercluster": map[string]any{}, "nvmfBasePort": 4421}))

	wantDenied(t, response, "a supercluster with nvmfBasePort 4421", "spec.nvmfBasePort")
}

// U-145, the supercluster half: every member names it in an immutable field,
// so deleting it would strand them.
func TestDeletingASuperclusterWithMembersIsRefused(t *testing.T) {
	prod := superclusterAt("sb", "prod")
	validator := clusterValidator(t, prod, memberAt("sb", "prod-dense-kqv", "prod"))

	response := validator.Handle(context.Background(), deleteClusterRequest(t, prod))

	wantDenied(t, response, "deleting a supercluster with a member", "prod-dense-kqv")
}

func TestDeletingAnEmptySuperclusterIsAdmitted(t *testing.T) {
	prod := superclusterAt("sb", "prod")
	validator := clusterValidator(t, prod, memberAt("sb", "staging-a-xyz", "staging"),
		memberAt("elsewhere", "prod-b-abc", "prod"))

	response := validator.Handle(context.Background(), deleteClusterRequest(t, prod))

	wantAllowed(t, response, "deleting a supercluster no member in its namespace names")
}

func TestDeletingAStandaloneClusterIsAdmitted(t *testing.T) {
	standalone := clusterAt("sb", "standalone")
	validator := clusterValidator(t, standalone)

	response := validator.Handle(context.Background(), deleteClusterRequest(t, standalone))

	wantAllowed(t, response, "deleting a standalone cluster")
}

// storageClusterSpecDefaultsForTest reads the defaults of spec's top-level
// fields from the embedded CRD, independently of the code under test.
func storageClusterSpecDefaultsForTest(t *testing.T) map[string]any {
	t.Helper()
	definitions, err := crds.Load()
	if err != nil {
		t.Fatalf("load the embedded CRDs: %v", err)
	}
	for _, d := range definitions {
		if d.Kind() != "StorageCluster" {
			continue
		}
		for _, version := range d.Object.Spec.Versions {
			if version.Name != "v1alpha2" {
				continue
			}
			out := map[string]any{}
			spec := version.Schema.OpenAPIV3Schema.Properties["spec"]
			for field, property := range spec.Properties {
				if property.Default == nil {
					continue
				}
				var value any
				if err := json.Unmarshal(property.Default.Raw, &value); err != nil {
					t.Fatalf("decode the default of %s: %v", field, err)
				}
				out[field] = value
			}
			return out
		}
	}
	t.Fatal("the embedded CRDs hold no v1alpha2 StorageCluster")
	return nil
}
