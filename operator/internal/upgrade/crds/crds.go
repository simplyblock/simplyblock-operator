// The CRDs this binary installs, carried inside it.
//
// §11 requires that the upgrade apply the CRDs itself rather than fetch them
// from a release tag, so that the version of the CRDs always matches the
// version of the conversion code that converts between them. A run that
// installed a schema its own converter had never seen would be converting
// blind.
//
// They are a committed copy of what controller-gen writes into
// config/crd/bases, because go:embed cannot reach outside the directory of the
// file that declares it. `make -C operator manifests` refreshes the copy, which
// is the same arrangement the Helm chart's copy of the same files uses.
//
// Which CRD belongs to which of §11's groups is read from the CRD rather than
// listed here. A table of kinds is a second place to edit when a kind is added,
// and the two disagree the first time somebody edits one of them.

package crds

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"
)

// manifests holds the generated CRDs. The pattern is deliberately narrow: a
// stray file in the directory would otherwise be embedded and then fail to
// parse as a CRD at run time, which is a failure the build should have had.
//
//go:embed manifests/*.yaml
var manifests embed.FS

// Shape is which of §11's groups a CRD is in, worked out from its content.
type Shape string

const (
	// ShapeConverting is a CRD whose kind exists in both versions, so the
	// upgrade serves both and hands conversion to the webhook.
	ShapeConverting Shape = "converting"

	// ShapeSingle is a CRD that serves one version, which is a kind that is
	// new in the target version and a kind that is not converting at all. They
	// are one shape because they are applied and verified identically: the
	// served version is the storage version and nothing converts.
	ShapeSingle Shape = "single"
)

// Definition is one CRD this binary carries.
type Definition struct {
	// Object is the CRD as it is to be applied.
	Object *apiextensionsv1.CustomResourceDefinition

	// Source is the embedded file, which is what an error names. A message
	// about storage.simplyblock.io_storagenodes.yaml is one somebody can act
	// on, and a message about entry seven is not.
	Source string
}

// Name is the CRD's name, which is also its identity in the cluster.
func (d Definition) Name() string { return d.Object.Name }

// Kind is the kind the CRD serves.
func (d Definition) Kind() string { return d.Object.Spec.Names.Kind }

// Group is the API group the CRD belongs to.
func (d Definition) Group() string { return d.Object.Spec.Group }

// Shape reports which of §11's groups this CRD is in.
//
// It is read from the versions the CRD declares. A kind that exists in two
// versions has to be converted between them, and a kind that exists in one
// does not, which is the whole of the distinction §11 draws.
func (d Definition) Shape() Shape {
	if len(d.Object.Spec.Versions) > 1 {
		return ShapeConverting
	}
	return ShapeSingle
}

// Served is the versions the CRD serves, in the order it declares them.
func (d Definition) Served() []string {
	out := make([]string, 0, len(d.Object.Spec.Versions))
	for _, version := range d.Object.Spec.Versions {
		if version.Served {
			out = append(out, version.Name)
		}
	}
	return out
}

// Storage is the version the API server persists, and reports whether the CRD
// names one. A CRD with no storage version is one the API server refuses, so
// the false is a broken embedded file rather than a state to handle.
func (d Definition) Storage() (string, bool) {
	for _, version := range d.Object.Spec.Versions {
		if version.Storage {
			return version.Name, true
		}
	}
	return "", false
}

// ConversionStrategy is how the API server converts between this CRD's
// versions. An unset strategy means None, which is what the API server
// defaults it to, so the two spellings are collapsed here rather than at every
// place that compares one.
func (d Definition) ConversionStrategy() apiextensionsv1.ConversionStrategyType {
	if d.Object.Spec.Conversion == nil || d.Object.Spec.Conversion.Strategy == "" {
		return apiextensionsv1.NoneConverter
	}
	return d.Object.Spec.Conversion.Strategy
}

// Load parses every embedded CRD, in name order.
//
// The order is fixed so that a plan lists them the same way twice, and by name
// rather than by file so that renaming a generated file does not reorder the
// plan.
func Load() ([]Definition, error) {
	entries, err := fs.ReadDir(manifests, manifestDir)
	if err != nil {
		return nil, fmt.Errorf("reading the embedded CRDs: %w", err)
	}

	out := make([]Definition, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		source := path.Join(manifestDir, entry.Name())
		raw, err := manifests.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", source, err)
		}

		definition, err := parse(raw, entry.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, definition)
	}

	if len(out) == 0 {
		// An empty set would make every step below it a silent no-operation,
		// and the upgrade would report that it had installed the CRDs.
		return nil, fmt.Errorf("no CRDs are embedded in this binary, so `make -C operator manifests` has not run")
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// manifestDir is where the copied CRDs live, named once so the embed pattern
// and the read agree.
const manifestDir = "manifests"

// parse reads one CRD.
func parse(raw []byte, source string) (Definition, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return Definition{}, fmt.Errorf("%s is not a CustomResourceDefinition: %w", source, err)
	}
	if crd.Name == "" {
		return Definition{}, fmt.Errorf("%s declares no name, so it is not a CRD", source)
	}
	if crd.Kind != "" && crd.Kind != "CustomResourceDefinition" {
		return Definition{}, fmt.Errorf("%s is a %s and not a CustomResourceDefinition", source, crd.Kind)
	}
	if len(crd.Spec.Versions) == 0 {
		return Definition{}, fmt.Errorf("%s declares no versions", source)
	}

	return Definition{Object: &crd, Source: source}, nil
}

// Matches reports whether an installed CRD is already the one this file
// describes.
//
// §11 refuses to write a CRD whose content already matches, because a CRD is
// what every custom resource of its kind is served through and a write that
// changes nothing can still fail. So the comparison has to be exact in both
// directions: treating a difference as a match leaves the wrong schema
// installed, and treating a match as a difference rewrites all of them on
// every run.
func (d Definition) Matches(live *apiextensionsv1.CustomResourceDefinition) bool {
	if live == nil {
		return false
	}
	return equality.Semantic.DeepEqual(normalized(d.Object.Spec), normalized(live.Spec))
}

// normalized fills in what the API server defaults, so that a CRD read back
// from the cluster compares equal to the file it was written from.
//
// One field needs it. An absent spec.conversion is stored as one saying
// None, which is the same CRD spelled the way the API server keeps it, and
// every other field of a controller-gen CRD round-trips unchanged. A later
// Kubernetes that defaults something else would show as every CRD wanting an
// update on every run, which is visible in the plan rather than silent.
func normalized(spec apiextensionsv1.CustomResourceDefinitionSpec) apiextensionsv1.CustomResourceDefinitionSpec {
	out := *spec.DeepCopy()
	if out.Conversion == nil {
		out.Conversion = &apiextensionsv1.CustomResourceConversion{}
	}
	if out.Conversion.Strategy == "" {
		out.Conversion.Strategy = apiextensionsv1.NoneConverter
	}
	return out
}
