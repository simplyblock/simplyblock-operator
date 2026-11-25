// The fields removed from the v1alpha2 API for having no reader, checked
// against the generated CRDs: a field that comes back in a schema is a setting
// the API accepts again and nothing acts on.

package bases

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRemovedFieldsAreGoneFromTheSchema(t *testing.T) {
	cases := []struct {
		crd  string
		path string // dotted, below openAPIV3Schema.properties
	}{
		// No reader in the operator, the CSI driver, or the support tool.
		{"storage.simplyblock.io_clusterdeploymentconfigs.yaml", "spec.edgeCluster"},
		// Copied from the deployment config onto the cluster and never sent to
		// the control plane, whose API has no such setting.
		{"storage.simplyblock.io_storageclusters.yaml", "spec.arbitration.holdMs"},
		{"storage.simplyblock.io_clusterdeploymentconfigs.yaml", "spec.cluster.arbitration.holdMs"},
		{"storage.simplyblock.io_storageclusters.yaml", "spec.arbitration.leaseTtlMs"},
		{"storage.simplyblock.io_clusterdeploymentconfigs.yaml", "spec.cluster.arbitration.leaseTtlMs"},
	}
	for _, c := range cases {
		raw, err := FS.ReadFile(c.crd)
		if err != nil {
			t.Fatal(err)
		}
		var crd map[string]any
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatal(err)
		}
		if field := schemaField(t, crd, "v1alpha2", c.path); field != nil {
			t.Errorf("%s: the v1alpha2 schema still has %s", c.crd, c.path)
		}
	}
}

// schemaField returns the schema of the dotted path in a version of crd, or
// nil when the path is not there.
func schemaField(t *testing.T, crd map[string]any, version, path string) any {
	t.Helper()
	versions, _ := dig(crd, "spec", "versions").([]any)
	for _, v := range versions {
		if dig(v, "name") != version {
			continue
		}
		node := dig(v, "schema", "openAPIV3Schema")
		for _, segment := range strings.Split(path, ".") {
			node = dig(node, "properties", segment)
			if node == nil {
				return nil
			}
		}
		return node
	}
	t.Fatalf("the CRD has no %s version", version)
	return nil
}

func dig(m any, keys ...string) any {
	for _, k := range keys {
		mm, _ := m.(map[string]any)
		m = mm[k]
	}
	return m
}
