// The chart's values as the editor offers them, and the checks a set of
// overrides meets.

package helmvalues

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
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

// image, the file's first value, has no comment of its own, so its help is
// empty rather than the license header above it.
func TestTheFirstValueDoesNotCarryTheFilesHeader(t *testing.T) {
	f := field(t, Fields(), "image")
	if f.Help != "" {
		t.Errorf("image's help is %q", f.Help)
	}
}

func TestASchemaCombinatorIsNotAFindingOfItsOwn(t *testing.T) {
	for _, f := range Check(map[string]any{"tls": map[string]any{"provider": "letsencrypt"}}) {
		if strings.Contains(f.Message, "'allOf' failed") || strings.Contains(f.Message, "'anyOf' failed") || strings.Contains(f.Message, "'oneOf' failed") {
			t.Errorf("a combinator wrapping the real findings is reported: %+v", f)
		}
	}
}

func TestAnEmptyMapIsKeyValueOnlyWhenTheSchemaSaysStrings(t *testing.T) {
	if f := field(t, Fields(), "discovery.nodeSelector"); f.Kind != "map" {
		t.Errorf("discovery.nodeSelector, a map of strings, is %s", f.Kind)
	}
	if f := field(t, Fields(), "driver.pnfs.mds.resources"); f.Kind != "yaml" {
		t.Errorf("driver.pnfs.mds.resources, which holds objects, is %s", f.Kind)
	}
}

func TestAnEmptyListIsLinesOnlyWhenTheSchemaSaysStrings(t *testing.T) {
	if f := field(t, Fields(), "driver.tolerations"); f.Kind != "yaml" {
		t.Errorf("driver.tolerations, a list of objects, is %s", f.Kind)
	}
}

func TestANullOverrideDeletesTheKeyAsHelmDoes(t *testing.T) {
	if found := Check(map[string]any{"discovery": map[string]any{"namespace": nil}}); len(found) != 0 {
		t.Errorf("a null that Helm reads as removing the default is refused: %+v", found)
	}
}

// fieldAt finds a field by its path segments, which a key holding dots needs.
func fieldAt(fields []Field, segments []string) *Field {
	for i := range fields {
		f := &fields[i]
		if len(f.Segments) > len(segments) || !slices.Equal(f.Segments, segments[:len(f.Segments)]) {
			continue
		}
		if len(f.Segments) == len(segments) {
			return f
		}
		return fieldAt(f.Children, segments)
	}
	return nil
}

func TestAKeyHoldingDotsIsOneSegment(t *testing.T) {
	segments := []string{"prometheus", "server", "podAnnotations", "reloader.stakater.com/auto"}
	f := fieldAt(Fields(), segments)
	if f == nil || f.Key != "reloader.stakater.com/auto" {
		t.Fatalf("no field at %v: %+v", segments, f)
	}

	overrides := map[string]any{}
	if err := SetYAML(overrides, segments, `"false"`); err != nil {
		t.Fatal(err)
	}
	annotations := overrides["prometheus"].(map[string]any)["server"].(map[string]any)["podAnnotations"].(map[string]any)
	if annotations["reloader.stakater.com/auto"] != "false" {
		t.Errorf("the value was set at %v", overrides)
	}
}
