// Rendering an object of the operator's API as a document to apply: YAML with
// its apiVersion and kind, and without the status and the metadata only an API
// server sets. Both editors hand their result to a customer to apply, so both
// render it the same way.

package manifest

import (
	"encoding/json"

	"sigs.k8s.io/yaml"
)

// Render is obj as YAML to apply.
func Render(obj any, apiVersion, kind string) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["apiVersion"], doc["kind"] = apiVersion, kind
	delete(doc, "status")
	if meta, ok := doc["metadata"].(map[string]any); ok {
		for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "managedFields", "generation"} {
			delete(meta, k)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return yaml.JSONToYAML(out)
}
