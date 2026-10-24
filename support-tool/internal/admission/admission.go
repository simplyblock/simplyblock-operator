// What the API server's admission says about a document, without an API
// server: the OpenAPI schema and the CEL rules of the operator's own CRDs,
// evaluated by the API server's own validators.
//
// The tool builds documents a support agent hands to a customer to apply, and
// a document the API server refuses is a round trip through the customer for a
// mistake the tool could have caught. The CRDs are the ones the operator
// embeds, so a rule added to the API reaches this check with the next build.
// Validating webhooks are not run: they read the cluster, which the bundle is a
// copy of, and the analysis runs the checks that matter against that copy.

package admission

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	celconfig "k8s.io/apiserver/pkg/apis/cel"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/crds"
)

// validator is what admission evaluates for one kind and version.
type validator struct {
	schema     validation.SchemaValidator
	structural *structuralschema.Structural
	cel        *cel.Validator
}

var (
	once       sync.Once
	loadErr    error
	validators = map[schema.GroupVersionKind]*validator{}
	scheme     = runtime.NewScheme()
)

// load builds a validator for every version of every CRD the operator embeds.
func load() {
	if loadErr = simplyblockv1alpha2.AddToScheme(scheme); loadErr != nil {
		return
	}
	definitions, err := crds.Load()
	if err != nil {
		loadErr = err
		return
	}
	for _, d := range definitions {
		for _, version := range d.Object.Spec.Versions {
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			var internal apiextensions.JSONSchemaProps
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
				version.Schema.OpenAPIV3Schema, &internal, nil); err != nil {
				loadErr = fmt.Errorf("%s %s: %w", d.Name(), version.Name, err)
				return
			}
			structural, err := structuralschema.NewStructural(&internal)
			if err != nil {
				loadErr = fmt.Errorf("%s %s: %w", d.Name(), version.Name, err)
				return
			}
			schemaValidator, _, err := validation.NewSchemaValidator(&internal)
			if err != nil {
				loadErr = fmt.Errorf("%s %s: %w", d.Name(), version.Name, err)
				return
			}
			gvk := schema.GroupVersionKind{Group: d.Group(), Version: version.Name, Kind: d.Kind()}
			validators[gvk] = &validator{
				schema:     schemaValidator,
				structural: structural,
				cel:        cel.NewValidator(structural, true, celconfig.PerCallLimit),
			}
		}
	}
}

// Validate returns every reason admission refuses the object, empty when it is
// admitted. The error reports an object of a kind the operator has no CRD for.
func Validate(obj runtime.Object) ([]string, error) {
	once.Do(load)
	if loadErr != nil {
		return nil, fmt.Errorf("the operator's CRDs do not load: %w", loadErr)
	}
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		kinds, _, err := scheme.ObjectKinds(obj)
		if err != nil || len(kinds) == 0 {
			return nil, fmt.Errorf("%T is not a kind of the operator's API", obj)
		}
		gvk = kinds[0]
	}
	v, ok := validators[gvk]
	if !ok {
		return nil, fmt.Errorf("the operator has no CRD for %s", gvk)
	}

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	content["apiVersion"], content["kind"] = gvk.GroupVersion().String(), gvk.Kind

	errs := validation.ValidateCustomResource(nil, content, v.schema)
	// The API server checks list types apart from the schema: a set holding an
	// entry twice, or a map list holding a key twice, is refused.
	errs = append(errs, listtype.ValidateListSetsAndMaps(nil, v.structural, content)...)
	celErrs, _ := v.cel.Validate(context.Background(), nil, v.structural, content, nil, celconfig.RuntimeCELCostBudget)
	errs = append(errs, celErrs...)
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out, nil
}
