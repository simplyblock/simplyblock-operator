// The simplyblock-operator chart's values, as the Helm values editor offers
// them: every value with its default and the comment values.yaml documents it
// with, and a check of a set of overrides against the chart's own schema.
//
// The chart's files are copies (see the chart target), because the tool is a
// module of its own; a test keeps them equal to the chart. The schema is
// checked with the JSON Schema library Helm uses, so a values file the editor
// passes is one `helm install` takes.

package helmvalues

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"
)

//go:embed chart/values.yaml
var valuesYAML []byte

//go:embed chart/values.schema.json
var schemaJSON []byte

//go:embed chart/Chart.yaml
var chartYAML []byte

// Field is one value of the chart.
type Field struct {
	Key string `json:"key"`
	// Path is the value's dotted path, for reading, and Segments its keys,
	// for addressing it: a key may hold dots of its own, such as an annotation
	// named reloader.stakater.com/auto.
	Path     string   `json:"path"`
	Segments []string `json:"segments"`
	// Kind is bool, string, int, number, list (of scalars), map (of
	// arbitrary keys), object (of the fields in Children), or `yaml` for a value
	// the editor takes as YAML.
	Kind     string   `json:"kind"`
	Default  any      `json:"default,omitempty"`
	Help     string   `json:"help,omitempty"`
	Enum     []string `json:"enum,omitempty"`
	Children []Field  `json:"children,omitempty"`
}

// Finding is one problem with a set of values. Severity is error for what
// `helm install` refuses and warning for what it takes and ignores.
type Finding struct {
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

// Chart is the chart's name and version.
func Chart() (name, version string) {
	var chart struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	_ = yaml.Unmarshal(chartYAML, &chart)
	return chart.Name, chart.Version
}

// Defaults is values.yaml.
func Defaults() map[string]any {
	values := map[string]any{}
	_ = yaml.Unmarshal(valuesYAML, &values)
	return values
}

var (
	fieldsOnce sync.Once
	fields     []Field
	schemaDoc  map[string]any
)

// Fields is every value of the chart, by top-level section, in the order
// values.yaml states them.
func Fields() []Field {
	fieldsOnce.Do(func() {
		_ = json.Unmarshal(schemaJSON, &schemaDoc)
		var doc yamlv3.Node
		if err := yamlv3.Unmarshal(valuesYAML, &doc); err != nil || len(doc.Content) == 0 {
			return
		}
		fields = fieldsOf(doc.Content[0], nil)
	})
	return fields
}

func fieldsOf(mapping *yamlv3.Node, prefix []string) []Field {
	var out []Field
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		segments := append(append([]string(nil), prefix...), key.Value)
		f := Field{Key: key.Value, Path: strings.Join(segments, "."), Segments: segments, Help: commentOf(key)}
		// The parser hands the file's header, the comment above its ---, to
		// the first key along with the key's own comment.
		if len(prefix) == 0 && i == 0 {
			f.Help = strings.TrimSpace(strings.TrimPrefix(f.Help, preamble()))
		}
		node := schemaAt(segments)
		if enum, ok := node["enum"].([]any); ok {
			for _, e := range enum {
				f.Enum = append(f.Enum, fmt.Sprint(e))
			}
		}
		switch value.Kind {
		case yamlv3.MappingNode:
			// key=value lines write strings, so an empty mapping is a map
			// only where the schema says its values are strings. Anything
			// else, such as a resources block, may hold objects.
			switch {
			case len(value.Content) > 0:
				f.Kind = "object"
				f.Children = fieldsOf(value, segments)
			case typeOf(node["additionalProperties"]) == "string":
				f.Kind = "map"
			default:
				f.Kind = "yaml"
			}
		case yamlv3.SequenceNode:
			// One entry per line writes strings, so a list is lines only where
			// its items are strings: by the schema, or, without an item schema,
			// by a default of strings. An empty one says nothing and may hold
			// objects, as tolerations do.
			f.Kind = "yaml"
			switch itemType := typeOf(node["items"]); {
			case itemType == "string":
				f.Kind = "list"
			case itemType == "" && len(value.Content) > 0 && scalarStrings(value):
				f.Kind = "list"
			}
		default:
			switch value.Tag {
			case "!!bool":
				f.Kind = "bool"
			case "!!int":
				f.Kind = "int"
			case "!!float":
				f.Kind = "number"
			default:
				f.Kind = "string"
			}
		}
		if f.Kind != "object" {
			var v any
			if err := value.Decode(&v); err == nil {
				f.Default = v
			}
		}
		out = append(out, f)
	}
	return out
}

// preamble is the comment values.yaml opens with, above its --- marker, as
// commentOf renders a comment.
func preamble() string {
	var lines []string
	for _, line := range strings.Split(string(valuesYAML), "\n") {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			return ""
		}
		line = strings.TrimPrefix(strings.TrimSpace(line), "#")
		lines = append(lines, strings.TrimPrefix(line, " "))
	}
	return strings.Join(lines, "\n")
}

// commentOf is the comment values.yaml writes above a key, without its marks.
func commentOf(key *yamlv3.Node) string {
	var lines []string
	for _, line := range strings.Split(key.HeadComment, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "#")
		lines = append(lines, strings.TrimPrefix(line, " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// typeOf is the type a schema node states, or empty for none.
func typeOf(node any) string {
	m, _ := node.(map[string]any)
	t, _ := m["type"].(string)
	return t
}

// scalarStrings reports whether every item of a sequence is a string scalar.
func scalarStrings(seq *yamlv3.Node) bool {
	for _, item := range seq.Content {
		if item.Kind != yamlv3.ScalarNode || item.Tag != "!!str" {
			return false
		}
	}
	return true
}

// schemaAt is the schema of a value, or nil where the schema says nothing.
func schemaAt(segments []string) map[string]any {
	node := schemaDoc
	for _, key := range segments {
		properties, _ := node["properties"].(map[string]any)
		next, ok := properties[key].(map[string]any)
		if !ok {
			return nil
		}
		node = next
	}
	return node
}

var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	compileErr  error
)

// Check is every problem with the overrides merged over the defaults: what the
// chart's schema refuses, which `helm install` refuses too, and a value the chart
// does not have, which `helm install` takes and nothing reads.
func Check(overrides map[string]any) []Finding {
	out := []Finding{}
	compileOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
		if err != nil {
			compileErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if compileErr = c.AddResource("values.schema.json", doc); compileErr != nil {
			return
		}
		compiled, compileErr = c.Compile("values.schema.json")
	})
	if compileErr != nil {
		return append(out, Finding{Severity: "warning", Message: "the chart's schema does not compile: " + compileErr.Error()})
	}

	merged, err := json.Marshal(merge(Defaults(), overrides))
	if err != nil {
		return append(out, Finding{Severity: "error", Message: err.Error()})
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(merged))
	if err != nil {
		return append(out, Finding{Severity: "error", Message: err.Error()})
	}
	if err := compiled.Validate(instance); err != nil {
		var invalid *jsonschema.ValidationError
		if !errors.As(err, &invalid) {
			return append(out, Finding{Severity: "error", Message: err.Error()})
		}
		for _, unit := range leaves(*invalid.BasicOutput()) {
			message := unit.Error.String()
			// allOf, anyOf, and oneOf report themselves beside the violations
			// they combine, which say what is wrong; the combinator says only
			// that something is.
			if strings.HasSuffix(message, "' failed") && strings.HasPrefix(message, "'") {
				continue
			}
			out = append(out, Finding{Severity: "error", Path: pathOf(unit.InstanceLocation), Message: message})
		}
	}
	for _, path := range unknown(overrides, Fields(), "") {
		out = append(out, Finding{Severity: "warning", Path: path,
			Message: "the chart has no value " + path + ": helm install takes it, and nothing reads it"})
	}
	return out
}

// leaves is the units of a validation output that carry an error.
func leaves(unit jsonschema.OutputUnit) []jsonschema.OutputUnit {
	var out []jsonschema.OutputUnit
	if unit.Error != nil && len(unit.Errors) == 0 {
		out = append(out, unit)
	}
	for _, child := range unit.Errors {
		out = append(out, leaves(child)...)
	}
	return out
}

// pathOf is a JSON pointer as the dotted path values are written in.
func pathOf(pointer string) string {
	return strings.ReplaceAll(strings.TrimPrefix(pointer, "/"), "/", ".")
}

// unknown is every override the chart's values do not have. Under a map, any
// key is the chart's, and under a value taken as YAML the editor does not look.
func unknown(overrides map[string]any, known []Field, prefix string) []string {
	byKey := map[string]Field{}
	for _, f := range known {
		byKey[f.Key] = f
	}
	var out []string
	for key, value := range overrides {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		f, ok := byKey[key]
		switch {
		case !ok:
			out = append(out, path)
		case f.Kind == "object":
			if nested, isMap := value.(map[string]any); isMap {
				out = append(out, unknown(nested, f.Children, path)...)
			}
		}
	}
	sort.Strings(out)
	return out
}

// merge is overrides laid over values the way Helm coalesces a values file:
// maps merge key by key, a null removes the key, and anything else replaces.
func merge(values, overrides map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = v
	}
	for k, v := range overrides {
		if v == nil {
			delete(out, k)
			continue
		}
		if base, ok := out[k].(map[string]any); ok {
			if over, ok := v.(map[string]any); ok {
				out[k] = merge(base, over)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// SetYAML parses a value the editor takes as YAML and sets it at its path, given
// as segments, in the overrides.
func SetYAML(overrides map[string]any, segments []string, text string) error {
	path := strings.Join(segments, ".")
	if len(segments) == 0 {
		return errors.New("a value needs a path")
	}
	var value any
	if err := yaml.Unmarshal([]byte(text), &value); err != nil {
		return fmt.Errorf("%s does not parse as YAML: %w", path, err)
	}
	node := overrides
	for _, key := range segments[:len(segments)-1] {
		next, ok := node[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[key] = next
		}
		node = next
	}
	node[segments[len(segments)-1]] = value
	return nil
}

// Import is a values file as overrides.
func Import(text string) (map[string]any, error) {
	overrides := map[string]any{}
	if err := yaml.Unmarshal([]byte(text), &overrides); err != nil {
		return nil, fmt.Errorf("the values file does not parse: %w", err)
	}
	return overrides, nil
}

// Render is the overrides as a values file.
func Render(overrides map[string]any) ([]byte, error) {
	if len(overrides) == 0 {
		return []byte("{}\n"), nil
	}
	return yaml.Marshal(overrides)
}
