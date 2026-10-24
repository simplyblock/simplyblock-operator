// The chart's values as the editor offers them, and the checks a set of
// overrides meets.

package helmvalues

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestTheCopiesAreTheChartsFiles(t *testing.T) {
	for _, name := range []string{"values.yaml", "values.schema.json", "Chart.yaml"} {
		chart, err := os.ReadFile(filepath.Join("..", "..", "..", "helm-charts", "charts", "simplyblock-operator", name))
		if err != nil {
			t.Fatal(err)
		}
		copied, err := os.ReadFile(filepath.Join("chart", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(chart, copied) {
			t.Errorf("chart/%s differs from the chart's: run make -C support-tool chart", name)
		}
	}
}

func TestTheChartIsNamed(t *testing.T) {
	if name, version := Chart(); name != "simplyblock-operator" || version == "" {
		t.Errorf("the chart is %q %q", name, version)
	}
}

func TestTheDefaultsAreValuesYAML(t *testing.T) {
	discovery, _ := Defaults()["discovery"].(map[string]any)
	if discovery["name"] != "initial-discovery" {
		t.Errorf("discovery.name defaults to %v", discovery["name"])
	}
}

func field(t *testing.T, fields []Field, path string) Field {
	t.Helper()
	for _, f := range fields {
		if f.Path == path {
			return f
		}
		if strings.HasPrefix(path, f.Path+".") {
			return field(t, f.Children, path)
		}
	}
	t.Fatalf("no field %s", path)
	return Field{}
}

func TestAFieldCarriesItsCommentAndItsKind(t *testing.T) {
	f := field(t, Fields(), "discovery.name")
	if f.Kind != "string" || f.Default != "initial-discovery" || !strings.Contains(f.Help, "What the run is called") {
		t.Errorf("discovery.name is %+v", f)
	}
	if f := field(t, Fields(), "discovery.enabled"); f.Kind != "bool" {
		t.Errorf("discovery.enabled is %+v", f)
	}
	if f := field(t, Fields(), "discovery.nodeSelector"); f.Kind != "map" {
		t.Errorf("discovery.nodeSelector is %+v", f)
	}
}

func TestAFieldCarriesTheSchemasEnum(t *testing.T) {
	f := field(t, Fields(), "tls.provider")
	if len(f.Enum) != 2 {
		t.Errorf("tls.provider is %+v", f)
	}
}

func severity(found []Finding, s, text string) bool {
	for _, f := range found {
		if f.Severity == s && strings.Contains(f.Path+" "+f.Message, text) {
			return true
		}
	}
	return false
}

func TestWhatTheSchemaRefusesIsAnError(t *testing.T) {
	found := Check(map[string]any{"discovery": map[string]any{"notAField": "x"}})
	if !severity(found, "error", "notAField") {
		t.Errorf("an unknown key in the closed discovery section is %+v", found)
	}
	found = Check(map[string]any{"tls": map[string]any{"provider": "letsencrypt"}})
	if !severity(found, "error", "tls") {
		t.Errorf("a provider outside the enum is %+v", found)
	}
}

func TestAValueTheChartDoesNotHaveIsAWarning(t *testing.T) {
	found := Check(map[string]any{"fooBar": true})
	if !severity(found, "warning", "fooBar") {
		t.Errorf("a value the chart does not have is %+v", found)
	}
	if found := Check(map[string]any{"discovery": map[string]any{"nodeSelector": map[string]any{"role": "storage"}}}); len(found) != 0 {
		t.Errorf("a key of a map value is reported: %+v", found)
	}
}

func TestTheDefaultsPassTheirOwnSchema(t *testing.T) {
	if found := Check(nil); len(found) != 0 {
		t.Errorf("the chart's own defaults are refused: %+v", found)
	}
}

func TestTheValuesFileIsTheOverridesOnly(t *testing.T) {
	out, err := Render(map[string]any{"discovery": map[string]any{"name": "first-run"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the values file does not parse: %v", err)
	}
	if len(doc) != 1 || doc["discovery"].(map[string]any)["name"] != "first-run" {
		t.Errorf("the values file is %s", out)
	}
}

func TestTheFirstValueCarriesItsOwnCommentNotTheFilesHeader(t *testing.T) {
	f := field(t, Fields(), "clusterDnsDomain")
	if !strings.HasPrefix(f.Help, "The cluster's DNS domain") || strings.Contains(f.Help, "SPDX") || strings.Contains(f.Help, "Copyright") {
		t.Errorf("clusterDnsDomain's help is %q", f.Help)
	}
}

func TestASchemaCombinatorIsNotAFindingOfItsOwn(t *testing.T) {
	for _, f := range Check(map[string]any{"tls": map[string]any{"provider": "letsencrypt"}}) {
		if strings.Contains(f.Message, "'allOf' failed") || strings.Contains(f.Message, "'anyOf' failed") || strings.Contains(f.Message, "'oneOf' failed") {
			t.Errorf("a combinator wrapping the real findings is reported: %+v", f)
		}
	}
}
