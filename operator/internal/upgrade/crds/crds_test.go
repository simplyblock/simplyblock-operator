// Tests for the CRDs the binary carries.
//
// They are about the embedded set itself and not about a fixture, because the
// thing that can go wrong is the copy: a `make manifests` that did not rerun
// leaves this package holding CRDs that the conversion code in the same binary
// was not built against, which is the one failure §11 embeds them to prevent.

package crds

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestLoad_ReadsEveryEmbeddedCRD(t *testing.T) {
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The count is not asserted. It changes whenever a kind is added, and a
	// test that has to be edited for that is a test that gets edited without
	// being read.
	if len(loaded) == 0 {
		t.Fatal("no CRDs are embedded")
	}

	for _, definition := range loaded {
		if definition.Group() != "storage.simplyblock.io" {
			t.Errorf("%s is in group %q", definition.Source, definition.Group())
		}
		if definition.Kind() == "" {
			t.Errorf("%s declares no kind", definition.Source)
		}
		if _, named := definition.Storage(); !named {
			t.Errorf("%s names no storage version, which the API server refuses", definition.Source)
		}
		if len(definition.Served()) == 0 {
			t.Errorf("%s serves no version", definition.Source)
		}
	}
}

func TestLoad_IsSortedByName(t *testing.T) {
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for i := 1; i < len(loaded); i++ {
		if loaded[i-1].Name() >= loaded[i].Name() {
			t.Fatalf("%s comes before %s", loaded[i-1].Name(), loaded[i].Name())
		}
	}
}

func TestMatches_HoldsAgainstWhatTheAPIServerDefaults(t *testing.T) {
	// A CRD read back from the cluster carries spec.conversion even where the
	// file said nothing, because the API server defaults it. Comparing the two
	// naively would report every CRD as needing an update on every run, and
	// §11 is specifically about not writing a CRD that already matches.
	definition := Definition{Object: &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group:    "storage.simplyblock.io",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true}},
		},
	}}

	stored := definition.Object.DeepCopy()
	stored.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}
	if !definition.Matches(stored) {
		t.Error("a CRD the API server defaulted does not match the file it was written from")
	}

	changed := definition.Object.DeepCopy()
	changed.Spec.Versions[0].Name = "v1alpha2"
	if definition.Matches(changed) {
		t.Error("a CRD serving another version matches")
	}

	if definition.Matches(nil) {
		t.Error("a CRD that is not installed matches")
	}
}

func TestMatches_SeesARealConversionStrategy(t *testing.T) {
	// Defaulting an unset strategy to None must not also collapse a real one,
	// which is how a converting CRD would look installed when its conversion
	// webhook had never been wired.
	definition := Definition{Object: &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true}},
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
			},
		},
	}}

	none := definition.Object.DeepCopy()
	none.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}
	if definition.Matches(none) {
		t.Error("a CRD that converts by nothing matches one that converts by webhook")
	}
}

func TestLoad_MatchesTheGeneratedCRDs(t *testing.T) {
	// The copy is generated, so the thing worth holding still is that it was
	// generated from the CRDs next door rather than edited here.
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, definition := range loaded {
		want := "storage.simplyblock.io_"
		if !strings.HasPrefix(strings.TrimPrefix(definition.Source, manifestDir+"/"), want) {
			t.Errorf("%s is not a generated CRD file name", definition.Source)
		}
	}
}

func TestShape_ReadsTheVersionsRatherThanAList(t *testing.T) {
	one := Definition{Object: &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha2"}},
		},
	}}
	two := Definition{Object: &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1"}, {Name: "v1alpha2"}},
		},
	}}

	if one.Shape() != ShapeSingle {
		t.Errorf("a CRD serving one version is %q", one.Shape())
	}
	if two.Shape() != ShapeConverting {
		t.Errorf("a CRD serving two versions is %q", two.Shape())
	}
}

func TestConversionStrategy_TreatsUnsetAsNone(t *testing.T) {
	// The API server defaults it, so a CRD read back from the cluster says
	// None where the file said nothing. Collapsing the two here is what keeps
	// every comparison from having to.
	unset := Definition{Object: &apiextensionsv1.CustomResourceDefinition{}}
	if unset.ConversionStrategy() != apiextensionsv1.NoneConverter {
		t.Errorf("an unset strategy reads as %q", unset.ConversionStrategy())
	}

	empty := Definition{Object: &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{},
		},
	}}
	if empty.ConversionStrategy() != apiextensionsv1.NoneConverter {
		t.Errorf("an empty strategy reads as %q", empty.ConversionStrategy())
	}
}

func TestParse_RefusesSomethingThatIsNotACRD(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"a Deployment", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: x\n", "is a Deployment"},
		{"no name", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n", "declares no name"},
		{
			"no versions",
			"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: x\n",
			"declares no versions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse([]byte(tc.raw), "test.yaml")
			if err == nil {
				t.Fatal("parse accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
