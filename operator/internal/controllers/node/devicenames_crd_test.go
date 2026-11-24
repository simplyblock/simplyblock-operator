// The StorageNode CRD as the API server enforces it: spec.config.deviceNames
// carries no immutability rule, because migration writes it. A CEL rule binds
// the operator as much as a user, so the rule would refuse the migration's
// patch; users are kept out by the StorageNode webhook instead.

package node

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestMigrationCanWriteTheDeviceNames(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "crd", "bases", "storage.simplyblock.io_storagenodes.yaml"))
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
		config := dig(v, "schema", "openAPIV3Schema", "properties", "spec", "properties", "config")
		field, _ := dig(config, "properties", "deviceNames").(map[string]any)
		if field == nil {
			t.Fatal("the v1alpha2 schema has no spec.config.deviceNames")
		}
		if rules, _ := field["x-kubernetes-validations"].([]any); len(rules) > 0 {
			t.Errorf("spec.config.deviceNames carries %v, which refuses the migration's patch", rules)
		}
		configRules, _ := dig(config, "x-kubernetes-validations").([]any)
		for _, r := range configRules {
			if rule, _ := r.(map[string]any)["rule"].(string); rule == "!has(oldSelf.deviceNames) || has(self.deviceNames)" {
				t.Errorf("spec.config carries %q, the once-set half of the immutability marker", rule)
			}
		}
		return
	}
	t.Fatal("the CRD has no v1alpha2 version")
}
