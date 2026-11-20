// The schema defaults of StorageCluster's spec, read from the CRD this binary
// carries, for the supercluster rule that admits a field at its default.
//
// The defaults are read from the generated schema rather than listed here, so
// a defaulted field added to the spec later is covered without a change to the
// rule. The embedded copy is the one internal/upgrade/crds already keeps in
// step with config/crd/bases, which is what the API server applies.

package webhook

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade/crds"
)

// storageClusterSpecDefaults returns the default of every top-level spec
// field that declares one, keyed by the field's JSON name.
var storageClusterSpecDefaults = sync.OnceValues(func() (map[string]json.RawMessage, error) {
	definitions, err := crds.Load()
	if err != nil {
		return nil, err
	}
	for _, d := range definitions {
		if d.Group() != "storage.simplyblock.io" || d.Kind() != "StorageCluster" {
			continue
		}
		for _, version := range d.Object.Spec.Versions {
			if version.Name != "v1alpha2" || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			out := map[string]json.RawMessage{}
			for field, property := range version.Schema.OpenAPIV3Schema.Properties["spec"].Properties {
				if property.Default != nil {
					out[field] = json.RawMessage(property.Default.Raw)
				}
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("the embedded CRDs hold no v1alpha2 StorageCluster schema")
})

// sameJSON reports whether two JSON values are equal as values, so that 4420
// and 4420.0, or two objects with their keys in another order, compare equal.
func sameJSON(a, b json.RawMessage) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}
