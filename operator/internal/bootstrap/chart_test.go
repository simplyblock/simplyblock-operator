// That the chart writes the document this package reads.
//
// The two halves of this are written in different languages and checked by
// nothing that compiles: the chart renders its `discovery` values verbatim into a
// ConfigMap, and the operator unmarshals that ConfigMap into Config. A key
// renamed on one side alone parses into a zero value on the other, which is a
// discovery run quietly doing something else, with no error, no event, and a
// draft that looks deliberate.
//
// So the chart's own values are parsed here, strictly, and a key this package
// does not know fails.
//
// Parsing the values alone is not enough, because it skips the template that
// carries them. What the operator reads is one ConfigMap, under one key, holding
// one values path, and all three are written in the template rather than in the
// values: renaming any of them leaves the values unchanged and the operator with
// nothing to read. TestTheTemplateWiresWhatThisPackageReads is that half.

package bootstrap

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// chartValues is the development chart, which is the one the release is cut from.
const chartValues = "../../../helm-charts/charts/simplyblock-operator/values.yaml"

func TestTheChartWritesTheDocumentThisPackageReads(t *testing.T) {
	raw, err := os.ReadFile(chartValues)
	if err != nil {
		t.Fatalf("reading the chart's values: %v", err)
	}

	// Only the discovery tree, because that is what the chart renders into the
	// ConfigMap: `bootstrap.yaml: {{ toYaml .Values.discovery }}`.
	var values struct {
		Discovery map[string]any `json:"discovery"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parsing the chart's values: %v", err)
	}
	if len(values.Discovery) == 0 {
		t.Fatal("the chart states no discovery values, so the ConfigMap it renders is empty")
	}

	document, err := yaml.Marshal(values.Discovery)
	if err != nil {
		t.Fatalf("re-rendering the discovery values: %v", err)
	}

	// Strict here and lenient in Load, and the two are the same decision read
	// twice. A key the operator does not know is a mistake when the chart beside
	// it wrote the key, and it is a newer chart when a cluster wrote it, so it
	// fails in the repository and is ignored in the field.
	config := &Config{}
	if err := yaml.UnmarshalStrict(document, config); err != nil {
		t.Fatalf("the chart writes a key this package does not read: %v", err)
	}

	// The defaults have to be the behavior the operator had before any of this
	// existed, or installing the chart changes what an upgrade does.
	if !config.DiscoveryEnabled() {
		t.Error("the chart's defaults raise no discovery run")
	}
	if got := config.RunName(theOperatorsOwnName); got != theOperatorsOwnName {
		t.Errorf("the chart names the run %q, want the operator's own constant", got)
	}
	if seed := config.Seed(); seed != nil {
		t.Errorf("the chart's defaults seed the draft with %+v, which overrides what "+
			"the run found on an installation that decided nothing", seed)
	}
	if images := config.DraftImages(); images != nil {
		t.Errorf("the chart's defaults pin %+v rather than leaving each image to the "+
			"pairing this release was tested as", images)
	}
}

// chartTemplate is the one that renders the ConfigMap this package reads.
const chartTemplate = "../../../helm-charts/charts/simplyblock-operator/templates/" +
	"bootstrap-configmap.yaml"

// The three names the template and this package have to agree on, none of which
// appears in the values.
//
// A rename on one side alone is silent in both directions. The operator reads
// ConfigMapName and ConfigKey as constants and finds nothing, which it treats as
// an installation that stated nothing; the chart renders a perfectly valid
// object nobody looks at. Neither is an error anywhere.
func TestTheTemplateWiresWhatThisPackageReads(t *testing.T) {
	raw, err := os.ReadFile(chartTemplate)
	if err != nil {
		t.Fatalf("reading the chart's bootstrap template: %v", err)
	}
	rendered := string(raw)

	for _, wanted := range []struct{ what, text string }{
		{"the ConfigMap this package looks for", "name: " + ConfigMapName},
		{"the key this package reads it under", ConfigKey + ":"},
		{"the values path the operator parses", ".Values.discovery"},
	} {
		if !strings.Contains(rendered, wanted.text) {
			t.Errorf("the template does not carry %s (%q), so the operator reads "+
				"an installation that stated nothing", wanted.what, wanted.text)
		}
	}

	// The managed profile has to say it raises no run rather than say nothing,
	// because an absent ConfigMap already means an installation that predates
	// this file and keeps the behavior it was installed with.
	if !strings.Contains(rendered, "enabled: false") {
		t.Error("the template renders no `enabled: false` branch, so a managed " +
			"installation is indistinguishable from one that predates the chart")
	}
}
