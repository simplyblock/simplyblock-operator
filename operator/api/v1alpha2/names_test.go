// §19's name bounds, read back out of the schemas the markers produce.
//
// The markers are the whole of the rule — there is no code to test — so what is
// worth holding is that every kind that needs one has one, and that the number
// is the same number everywhere. A bound applied to the kinds somebody
// remembered is the state this test was written to end: ClusterDeploymentConfig
// carried MaxLength markers while StoragePool and StorageNode carried none, and
// nothing said the three were the same rule.
//
// It reads config/crd/bases rather than the Go source, because a marker that
// does not reach the schema is not a bound. The enumeration is the assertion: a
// kind added later with an unbounded reference fails this without anybody
// extending it.

package v1alpha2

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/atlas/kube"
)

// nameBound is what a name a label carries may be. It is a label's own limit
// rather than a budget: a reference inside it can still overflow a key that
// joins it to a namespace and a pool, and what the bound closes is every row
// where the name stands alone.
const nameBound = int64(kube.MaxLabelValueLength)

// labelBoundKinds are the kinds whose metadata.name is written into a label
// value somewhere, together with the write that binds it. A name that overflows
// there is not a rejected resource but a reconcile that retries forever, since
// the refusal lands on the derived write and the object being reconciled says
// nothing about the name that caused it.
var labelBoundKinds = map[string]string{
	"StorageCluster": "storage.simplyblock.io/cluster on a StorageClass and on a StorageDevice, " +
		"and io.simplyblock.storagenodeset on every worker Node the cluster claims",
	"StoragePool": "storage.simplyblock.io/pool, which is also the selector the pool " +
		"lists its own classes with",
	"StorageNode": "storage.simplyblock.io/node on every StorageDevice the mirror writes",
}

// generatedCRDs reads every CRD this repository generates, by kind.
func generatedCRDs(t *testing.T) map[string]*apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	dir := filepath.Join("..", "..", "config", "crd", "bases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the generated CRDs: %v", err)
	}

	out := make(map[string]*apiextensionsv1.CustomResourceDefinition)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}
		out[crd.Spec.Names.Kind] = &crd
	}
	if len(out) == 0 {
		t.Fatal("no CRDs were read, so every assertion below would pass vacuously")
	}
	return out
}

// v1alpha2Schema returns a kind's v1alpha2 schema, or nil for a kind that has no such
// version. A kind the redesign has not reached carries no rule (§19.9).
func v1alpha2Schema(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.JSONSchemaProps {
	for i := range crd.Spec.Versions {
		version := &crd.Spec.Versions[i]
		if version.Name == "v1alpha2" && version.Schema != nil {
			return version.Schema.OpenAPIV3Schema
		}
	}
	return nil
}

// becomesAClusterName are the fields that are not a reference but a name: the
// value one kind carries becomes another object's metadata.name, so it is held
// to what that name may be. Admitting more is a document the API server accepts
// and a creation step that can never succeed.
var becomesAClusterName = map[string][]string{
	"ClusterDeploymentConfig": {"cluster", "name"},
}

// property walks a path of property names, so a field nested inside a template
// can be named without the test knowing how the schema is shaped.
func property(
	node apiextensionsv1.JSONSchemaProps, path []string,
) (apiextensionsv1.JSONSchemaProps, error) {
	at := node
	for i, name := range path {
		child, ok := at.Properties[name]
		if !ok {
			return at, fmt.Errorf("no property %s under spec.%s",
				name, strings.Join(path[:i], "."))
		}
		at = child
	}
	return at, nil
}

// clusterRefs walks a schema and reports every property called clusterRef under
// it, by the path it sits at. The walk is recursive because a reference is not
// always a spec's own field: a discovery draft carries one inside the action it
// describes, and a rule written for the top level alone would leave that one
// unbounded while reading as though it covered everything.
func clusterRefs(
	path string, node apiextensionsv1.JSONSchemaProps, into map[string]apiextensionsv1.JSONSchemaProps,
) {
	for name, child := range node.Properties {
		where := path + "." + name
		if name == "clusterRef" {
			into[where] = child
		}
		clusterRefs(where, child, into)
	}
	if node.Items != nil && node.Items.Schema != nil {
		clusterRefs(path+"[]", *node.Items.Schema, into)
	}
}

// TestEveryClusterReferenceIsBoundedByWhatAClusterNameMayBe covers every
// reference to a StorageCluster the group's specs carry.
//
// The bound on the reference follows from the bound on the name rather than
// from anything about the referring kind: a reference longer than a
// StorageCluster name may be names nothing that can exist, so it is refused at
// admission rather than resolved forever by a controller that will never find
// it.
//
// Only spec is walked. A status carrying the same reference is a record of
// what the operator resolved, copied from an input this rule already bounds, so
// a maximum there could not catch a mistake and could only turn a status write
// into one the API server refuses.
func TestEveryClusterReferenceIsBoundedByWhatAClusterNameMayBe(t *testing.T) {
	var found int
	for kind, crd := range generatedCRDs(t) {
		root := v1alpha2Schema(crd)
		if root == nil {
			continue
		}
		spec, ok := root.Properties["spec"]
		if !ok {
			continue
		}

		refs := map[string]apiextensionsv1.JSONSchemaProps{}
		clusterRefs("spec", spec, refs)
		if named, ok := becomesAClusterName[kind]; ok {
			at, err := property(spec, named)
			if err != nil {
				t.Errorf("%s: %v", kind, err)
			} else {
				refs["spec."+strings.Join(named, ".")] = at
			}
		}
		for where, ref := range refs {
			found++
			switch {
			case ref.MaxLength == nil:
				t.Errorf("%s.%s carries no maxLength, so it admits 253 characters of a "+
					"StorageCluster name that could never be that long", kind, where)
			case *ref.MaxLength != nameBound:
				t.Errorf("%s.%s is bounded at %d and a StorageCluster name at %d; the two "+
					"are one rule, and a field bounded above the name is not bounded",
					kind, where, *ref.MaxLength, nameBound)
			}
		}
	}

	if found == 0 {
		t.Fatal("no kind declares a clusterRef, which is not what this group looks like")
	}
}

// TestANameALabelCarriesIsBoundedAtALabelsLimit covers the kinds whose own
// metadata.name travels into a label value.
//
// metadata.name takes no MaxLength marker, because it is not this schema's
// field: it is one of the two metadata fields a CRD validation rule can see
// (§19.7), so the bound is a rule on the type rather than a marker on a
// property.
func TestANameALabelCarriesIsBoundedAtALabelsLimit(t *testing.T) {
	all := generatedCRDs(t)
	for kind, written := range labelBoundKinds {
		crd, ok := all[kind]
		if !ok {
			t.Errorf("%s has no CRD, and it is listed here as a kind whose name is written "+
				"into %s", kind, written)
			continue
		}
		root := v1alpha2Schema(crd)
		if root == nil {
			t.Errorf("%s has no v1alpha2 schema to carry the rule", kind)
			continue
		}

		var bounded bool
		for _, rule := range root.XValidations {
			if strings.Contains(rule.Rule, "self.metadata.name") &&
				strings.Contains(rule.Rule, "63") {
				bounded = true
				break
			}
		}
		if !bounded {
			t.Errorf("%s carries no rule bounding self.metadata.name, and its name is "+
				"written into %s, where a label stops at %d bytes",
				kind, written, nameBound)
		}
	}
}
