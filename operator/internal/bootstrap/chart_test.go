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

package bootstrap

import (
	"os"
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
	if got := config.RunName("initial-discovery"); got != "initial-discovery" {
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
