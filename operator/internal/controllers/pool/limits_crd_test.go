// The StoragePool CRD as the console reads it: the pool-wide limits that cannot
// be set carry an immutability rule, so the Control Center shows them locked
// rather than offering an edit the API server refuses. The CEL rule on
// PoolLimits is what refuses them; this checks the generated manifest, which
// is all the console sees.

package pool

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// limitsProperty is the v1alpha2 schema of spec.limits.<name> in the
// generated StoragePool CRD.
func limitsProperty(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "crd", "bases", "storage.simplyblock.io_storagepools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	dig := func(m any, keys ...string) any {
		for _, k := range keys {
			mm, _ := m.(map[string]any)
			m = mm[k]
		}
		return m
	}
	for _, v := range dig(crd, "spec", "versions").([]any) {
		if dig(v, "name") != "v1alpha2" {
			continue
		}
		prop, _ := dig(v, "schema", "openAPIV3Schema", "properties", "spec", "properties", "limits", "properties", name).(map[string]any)
		if prop == nil {
			t.Fatalf("the v1alpha2 schema has no spec.limits.%s", name)
		}
		return prop
	}
	t.Fatal("the CRD has no v1alpha2 version")
	return nil
}

func isImmutable(prop map[string]any) bool {
	rules, _ := prop["x-kubernetes-validations"].([]any)
	for _, r := range rules {
		if rule, _ := r.(map[string]any)["rule"].(string); rule == "self == oldSelf" {
			return true
		}
	}
	return false
}

func TestTheLimitsThatCannotBeSetAreImmutable(t *testing.T) {
	for _, name := range []string{"iops", "throughput"} {
		if !isImmutable(limitsProperty(t, name)) {
			t.Errorf("spec.limits.%s carries no self == oldSelf rule, so the console offers an edit the API server refuses", name)
		}
	}
}
