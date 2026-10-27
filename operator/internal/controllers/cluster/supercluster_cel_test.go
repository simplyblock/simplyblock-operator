// The CEL rules that make a StorageCluster a supercluster or a member, run
// against a real apiserver (design-superclusters.md §4.1, Appendix A).
//
// The objects are unstructured on purpose. The rules decide what a
// supercluster may omit and which fields are fixed at creation, and an object
// built from a field the schema does not know yet is pruned rather than
// refused, so a typed object would let the test pass against a CRD that has
// none of the rules.

package cluster

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var storageClusterGVK = schema.GroupVersionKind{
	Group: "storage.simplyblock.io", Version: "v1alpha2", Kind: "StorageCluster",
}

// sized is the spec every cluster that is not a supercluster has to carry.
func sized() map[string]any {
	return map[string]any{"maxSubsystemCount": int64(10), "vcpuCount": int64(6)}
}

func unstructuredCluster(prefix string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(storageClusterGVK)
	u.SetGenerateName(prefix)
	u.SetNamespace("default")
	u.Object["spec"] = spec
	return u
}

func mergeSpec(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestSuperclusterCELCreateRules(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		spec       map[string]any
		wantDenied string
	}{
		// U-142: a supercluster carries no sizing, because it has no nodes.
		{name: "a supercluster without sizing", spec: map[string]any{
			"supercluster": map[string]any{},
		}},
		{name: "a supercluster with a placement strategy", spec: map[string]any{
			"supercluster": map[string]any{
				"placement": map[string]any{"strategy": "LeastProvisioned"},
			},
		}},
		// U-37: the relaxation is for superclusters only.
		{name: "a standalone cluster without maxSubsystemCount",
			spec:       map[string]any{"vcpuCount": int64(6)},
			wantDenied: "maxSubsystemCount and vcpuCount are required unless the cluster is a supercluster"},
		{name: "a standalone cluster without vcpuCount",
			spec:       map[string]any{"maxSubsystemCount": int64(10)},
			wantDenied: "maxSubsystemCount and vcpuCount are required unless the cluster is a supercluster"},
		{name: "a member", spec: mergeSpec(sized(), map[string]any{
			"superclusterRef": "prod",
		})},
		{name: "a member out of placement", spec: mergeSpec(sized(), map[string]any{
			"superclusterRef": "prod", "disablePlacement": true,
		})},
		// U-141
		{name: "a supercluster that is also a member", spec: map[string]any{
			"supercluster": map[string]any{}, "superclusterRef": "prod",
		}, wantDenied: "a supercluster cannot be a member of another"},
		// U-143
		{name: "disablePlacement on a standalone cluster",
			spec:       mergeSpec(sized(), map[string]any{"disablePlacement": true}),
			wantDenied: "disablePlacement is valid only on a member"},
		{name: "an unknown placement strategy", spec: map[string]any{
			"supercluster": map[string]any{
				"placement": map[string]any{"strategy": "RoundRobin"},
			},
		}, wantDenied: "Unsupported value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := unstructuredCluster("sc-cel-", tc.spec)
			err := apiClient.Create(context.Background(), cluster)
			if tc.wantDenied != "" {
				if err == nil {
					t.Fatal("the apiserver admitted a cluster it should have refused")
				}
				if !strings.Contains(err.Error(), tc.wantDenied) {
					t.Fatalf("refused for the wrong reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the apiserver refused a valid cluster: %v", err)
			}
			if err := apiClient.Delete(context.Background(), cluster); err != nil {
				t.Errorf("cleaning up the cluster: %v", err)
			}
		})
	}
}

// U-144: what a cluster is within a supercluster is decided when it is
// created, because every member names its supercluster and a supercluster's
// identifier is what StorageClasses name.
func TestSuperclusterCELRoleIsFixedAtCreation(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name       string
		initial    map[string]any
		mutate     func(spec map[string]any)
		wantDenied string
	}{
		{name: "adding supercluster to a standalone cluster", initial: sized(),
			mutate: func(spec map[string]any) {
				spec["supercluster"] = map[string]any{}
			},
			wantDenied: "a cluster cannot become a supercluster, or stop being one, after creation"},
		{name: "removing supercluster", initial: map[string]any{"supercluster": map[string]any{}},
			mutate: func(spec map[string]any) {
				delete(spec, "supercluster")
				spec["maxSubsystemCount"] = int64(10)
				spec["vcpuCount"] = int64(6)
			},
			wantDenied: "a cluster cannot become a supercluster, or stop being one, after creation"},
		{name: "adding superclusterRef", initial: sized(),
			mutate: func(spec map[string]any) {
				spec["superclusterRef"] = "prod"
			},
			wantDenied: "superclusterRef is set at creation and never changed"},
		{name: "changing superclusterRef",
			initial: mergeSpec(sized(), map[string]any{"superclusterRef": "prod"}),
			mutate: func(spec map[string]any) {
				spec["superclusterRef"] = "staging"
			},
			wantDenied: "superclusterRef is set at creation and never changed"},
		{name: "removing superclusterRef",
			initial: mergeSpec(sized(), map[string]any{"superclusterRef": "prod"}),
			mutate: func(spec map[string]any) {
				delete(spec, "superclusterRef")
			},
			wantDenied: "superclusterRef is set at creation and never changed"},
		{name: "changing the placement strategy",
			initial: map[string]any{"supercluster": map[string]any{}},
			mutate: func(spec map[string]any) {
				spec["supercluster"] = map[string]any{
					"placement": map[string]any{"strategy": "LeastVolumes"},
				}
			}},
		{name: "taking a member out of placement",
			initial: mergeSpec(sized(), map[string]any{"superclusterRef": "prod"}),
			mutate: func(spec map[string]any) {
				spec["disablePlacement"] = true
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cluster := unstructuredCluster("sc-cel-update-", tc.initial)
			if err := apiClient.Create(ctx, cluster); err != nil {
				t.Fatalf("creating the cluster to edit: %v", err)
			}
			t.Cleanup(func() { _ = apiClient.Delete(ctx, cluster) })

			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(storageClusterGVK)
			if err := apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), fresh); err != nil {
				t.Fatalf("reading the cluster back: %v", err)
			}
			spec, _ := fresh.Object["spec"].(map[string]any)
			tc.mutate(spec)

			err := apiClient.Update(ctx, fresh)
			if tc.wantDenied == "" {
				if err != nil {
					t.Fatalf("the apiserver refused a valid edit: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the apiserver admitted an edit it should have refused")
			}
			if !strings.Contains(err.Error(), tc.wantDenied) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
}
