// Command gen writes validation.gen.go: the UnmarshalJSON methods that make
// every control-plane response type validate itself as it is deserialized, and
// the rule tables they validate against (see ../validation.go for why, and
// ../validation.yaml for what).
//
// Which types to generate for is read off the generated client, so this needs
// none of oapi-codegen's schema-name-to-Go-name knowledge: they are the types a
// generated XxxResponse struct exposes as a JSON2xx payload, plus every model
// type reachable from one.
//
// The rules are checked against the OpenAPI spec, which is the authority on
// what the control plane's models, fields and operations are called: a rule
// naming a schema, property or endpoint the spec no longer has means it was
// renamed or removed, and a rule that can never fire is worse than no rule, so
// generation fails and the rules get fixed. Their keys are then resolved to Go
// fields through the generated struct tags.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"log"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// payloadField matches the fields a generated response struct exposes for a
// success body: JSON200, JSON201, … Error bodies (JSON422) are deliberately
// not validated — a strict decode there would mask the error it carries.
var payloadField = regexp.MustCompile(`^JSON2[0-9]{2}$`)

// goTypeName matches an exported Go type name, which is what an endpoint-scoped
// variant's `as` has to be.
var goTypeName = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

const (
	// presentRule means "this key must appear in the body"; it is checked
	// against the raw body rather than by the validator.
	presentRule = "present"
	// asKey names the Go type an endpoint-scoped variant compiles to. It is
	// the one key in a variant's block that is not a wire key.
	asKey = "as"
)

func main() {
	in := flag.String("in", "cpapi.gen.go", "generated client to read the response types from")
	specPath := flag.String("spec", "../../../shared/openapi.json", "OpenAPI spec to check the rules against")
	rules := flag.String("rules", "validation.yaml", "response validation rules to compile in")
	out := flag.String("out", "validation.gen.go", "file to write")
	flag.Parse()

	file, err := parser.ParseFile(token.NewFileSet(), *in, nil, parser.SkipObjectResolution)
	if err != nil {
		log.Fatalf("parse %s: %v", *in, err)
	}
	structs, unmarshalers := index(file)
	types := responseTypes(structs, unmarshalers)
	if len(types) == 0 {
		log.Fatalf("%s: found no response types — has the generated client changed shape?", *in)
	}

	spec, err := readSpec(*specPath)
	if err != nil {
		log.Fatalf("read %s: %v", *specPath, err)
	}
	byKey, err := os.ReadFile(*rules)
	if err != nil {
		log.Fatalf("read %s: %v", *rules, err)
	}
	compiled, err := compile(byKey, structs, spec, *specPath)
	if err != nil {
		log.Fatalf("%s: %v", *rules, err)
	}

	src, err := render(file.Name.Name, *in, *rules, types, compiled)
	if err != nil {
		log.Fatalf("render %s: %v", *out, err)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	variants := 0
	for _, r := range compiled {
		if r.base != "" {
			variants++
		}
	}
	fmt.Printf("%s: %d response types validate themselves, %d of them against rules (%d endpoint-scoped)\n",
		*out, len(types)+variants, len(compiled), variants)
}

// index returns the file's struct types by name, and the names of the types
// that already declare an UnmarshalJSON method.
func index(file *ast.File) (structs map[string]*ast.StructType, unmarshalers map[string]bool) {
	structs, unmarshalers = map[string]*ast.StructType{}, map[string]bool{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if st, ok := ts.Type.(*ast.StructType); ok {
					structs[ts.Name.Name] = st
				}
			}
		case *ast.FuncDecl:
			if d.Name.Name == "UnmarshalJSON" && d.Recv != nil && len(d.Recv.List) == 1 {
				unmarshalers[typeName(d.Recv.List[0].Type)] = true
			}
		}
	}
	return structs, unmarshalers
}

// responseTypes is the sorted set of model types to generate for: the success
// payloads of every response struct, plus every model type reachable from one.
func responseTypes(structs map[string]*ast.StructType, unmarshalers map[string]bool) []string {
	found := map[string]bool{}
	var reach func(name string)
	reach = func(name string) {
		st, ok := structs[name]
		if !ok || found[name] {
			return // not a model of ours, or already walked
		}
		found[name] = true
		for _, f := range st.Fields.List {
			reach(typeName(f.Type))
		}
	}
	for _, st := range structs {
		for _, f := range st.Fields.List {
			if len(f.Names) == 1 && payloadField.MatchString(f.Names[0].Name) {
				reach(typeName(f.Type))
			}
		}
	}

	types := make([]string, 0, len(found))
	for name := range found {
		// A response struct reached through a payload field (they nest) is
		// plumbing, not a model; the union types decode themselves.
		if unmarshalers[name] || strings.HasSuffix(name, "Response") {
			continue
		}
		types = append(types, name)
	}
	slices.Sort(types)
	return types
}

// typeName is the name of the type an expression ultimately refers to, looking
// through the pointers, slices and maps the generated models wrap them in. It
// is empty for anything else (qualified types from other packages, interfaces,
// inline structs), which is what keeps the walk inside this package.
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.ArrayType:
		return typeName(t.Elt)
	case *ast.MapType:
		return typeName(t.Value)
	default:
		return ""
	}
}

// rule is one type's compiled rules: validator tags by Go field name, and the
// JSON keys that must be present in the body. For an endpoint-scoped variant,
// base is the schema it is a flavor of and endpoint is where it comes from;
// both are empty for a schema's own rules.
type rule struct {
	typ      string
	tags     map[string]string
	keys     []string
	order    []string          // field names, in the order their keys appear in the YAML
	wire     map[string]string // Go field to the wire key it came from, for messages
	base     string
	endpoint string
}

// schema is as much of an OpenAPI schema as checking the rules needs.
type schema struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

// spec is as much of an OpenAPI document as checking the rules needs: the
// models, and the operations that can be scoped to.
type spec struct {
	Components struct {
		Schemas map[string]schema `json:"schemas"`
	} `json:"components"`
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

// readSpec reads the spec — the authority on what the control plane's models
// are called, which of their properties it promises to send, and which
// endpoints exist to scope a rule to.
func readSpec(path string) (spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return spec{}, err
	}
	var doc spec
	if err := json.Unmarshal(raw, &doc); err != nil {
		return spec{}, err
	}
	if len(doc.Components.Schemas) == 0 {
		return spec{}, errors.New("no component schemas, so this is not the OpenAPI document the client was generated from")
	}
	return doc, nil
}

// block is one entry of the rules file: a schema's rules, or an endpoint-scoped
// variant of them (`Schema@METHOD path`).
type block struct {
	schema   string
	endpoint string // empty for a schema's own rules
	line     int
	body     *yaml.Node
}

// compile turns the rules file into one rule per generated type, checking as it
// goes that everything it names still exists.
//
// Schema blocks come first regardless of where they sit in the file, because a
// variant compiles to the base's rules with its own applied on top: rules are
// registered per Go type and a variant is a distinct type, so it cannot inherit
// anything at runtime and has to carry the merged set.
func compile(rulesYAML []byte, structs map[string]*ast.StructType, doc spec, specPath string) ([]rule, error) {
	blocks, err := parseBlocks(rulesYAML)
	if err != nil {
		return nil, err
	}

	rules := make([]rule, 0, len(blocks))
	bases := make(map[string]rule, len(blocks))
	for _, b := range blocks {
		if b.endpoint != "" {
			continue
		}
		if _, dup := bases[b.schema]; dup {
			return nil, fmt.Errorf("line %d: a second %s block; merge it into the first, or the later one silently wins",
				b.line, b.schema)
		}
		r, err := compileRules(b, b.schema, structs, doc, specPath)
		if err != nil {
			return nil, err
		}
		bases[b.schema] = r
		rules = append(rules, r)
	}
	for _, b := range blocks {
		if b.endpoint == "" {
			continue
		}
		// Without the base's block there is nothing to merge, and the variant
		// would quietly enforce only its own rules — the shared ones would look
		// inherited and not be. An empty `<schema>: {}` says "no shared rules"
		// deliberately.
		base, ok := bases[b.schema]
		if !ok {
			return nil, fmt.Errorf("line %d: %s@%s has no %s block to extend — an endpoint-scoped block compiles to the base's rules plus its own, so add one (`%s: {}` if there are none to share)",
				b.line, b.schema, b.endpoint, b.schema, b.schema)
		}
		r, err := compileVariant(b, base, structs, doc, specPath)
		if err != nil {
			return nil, err
		}
		if _, taken := structs[r.typ]; taken {
			return nil, fmt.Errorf("line %d: %s is already a type of the generated client", b.line, r.typ)
		}
		if slices.ContainsFunc(rules, func(o rule) bool { return o.typ == r.typ }) {
			return nil, fmt.Errorf("line %d: %s is already generated for another block", b.line, r.typ)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// parseBlocks reads the rules file into blocks, keeping the file's order, which
// keeps the generated output stable and reviewable against the YAML.
func parseBlocks(rulesYAML []byte) ([]block, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(rulesYAML, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: expected a mapping of response type to rules", root.Line)
	}

	blocks := make([]block, 0, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, body := root.Content[i], root.Content[i+1]
		if body.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("line %d: expected a mapping of JSON key to rule under %s", body.Line, key.Value)
		}
		name, endpoint, _ := strings.Cut(key.Value, "@")
		blocks = append(blocks, block{
			schema:   strings.TrimSpace(name),
			endpoint: strings.TrimSpace(endpoint),
			line:     key.Line,
			body:     body,
		})
	}
	return blocks, nil
}

// compileRules resolves one block's wire keys against the schema and the
// generated struct, both of which must still have them.
func compileRules(b block, name string, structs map[string]*ast.StructType, doc spec, specPath string) (rule, error) {
	s, ok := doc.Components.Schemas[name]
	if !ok {
		return rule{}, fmt.Errorf("line %d: %s is not a schema in %s — the control plane renamed or removed it, so this rule can no longer fire",
			b.line, name, specPath)
	}
	st, ok := structs[name]
	if !ok {
		return rule{}, fmt.Errorf("line %d: %s is a schema in %s but this client was not generated for it",
			b.line, name, specPath)
	}

	r := rule{typ: name, tags: map[string]string{}, wire: map[string]string{}}
	for j := 0; j+1 < len(b.body.Content); j += 2 {
		keyNode, tag := b.body.Content[j], b.body.Content[j+1].Value
		key := keyNode.Value
		if key == asKey {
			if b.endpoint == "" {
				return rule{}, fmt.Errorf("line %d: %s is only for an endpoint-scoped block (Schema@METHOD path)", keyNode.Line, asKey)
			}
			continue // handled by compileVariant
		}
		if _, ok := s.Properties[key]; !ok {
			return rule{}, fmt.Errorf("line %d: %s has no property %q in %s — it was renamed or removed, so this rule can no longer fire",
				keyNode.Line, name, key, specPath)
		}
		field, ok := fieldForKey(st, key)
		if !ok {
			return rule{}, fmt.Errorf("line %d: %s.%s is in %s but this client does not decode it",
				keyNode.Line, name, key, specPath)
		}
		tag, present := cutPresent(tag)
		if present && !slices.Contains(s.Required, key) {
			return rule{}, fmt.Errorf("line %d: %s.%s is optional in %s, so it cannot be required to be present",
				keyNode.Line, name, key, specPath)
		}
		if present {
			r.keys = append(r.keys, key)
		}
		if tag != "" {
			r.tags[field] = tag
			r.wire[field] = key
			r.order = append(r.order, field)
		}
	}
	return r, nil
}

// compileVariant compiles an endpoint-scoped block: the base schema's rules
// with this endpoint's applied on top, under the Go type its `as` names.
//
// The endpoint has to exist in the spec, but the spec cannot confirm that it
// answers with this schema — the endpoints whose promises diverge from the
// shared model are exactly the ones FastAPI declares no response model for. The
// association is this client's claim; what generation checks is that everything
// the claim names is still there.
func compileVariant(b block, base rule, structs map[string]*ast.StructType, doc spec, specPath string) (rule, error) {
	method, path, ok := strings.Cut(b.endpoint, " ")
	if !ok {
		return rule{}, fmt.Errorf("line %d: %q is not a `METHOD path` endpoint", b.line, b.endpoint)
	}
	operations, ok := doc.Paths[path]
	if !ok {
		return rule{}, fmt.Errorf("line %d: %s is not a path in %s — it was renamed or removed, so this rule can no longer fire",
			b.line, path, specPath)
	}
	if _, ok := operations[strings.ToLower(method)]; !ok {
		return rule{}, fmt.Errorf("line %d: %s has no %s operation in %s", b.line, path, strings.ToUpper(method), specPath)
	}

	as := ""
	for j := 0; j+1 < len(b.body.Content); j += 2 {
		if b.body.Content[j].Value == asKey {
			as = b.body.Content[j+1].Value
		}
	}
	if !goTypeName.MatchString(as) {
		return rule{}, fmt.Errorf("line %d: an endpoint-scoped block needs `%s: <ExportedGoTypeName>` to compile to, got %q",
			b.line, asKey, as)
	}

	own, err := compileRules(b, b.schema, structs, doc, specPath)
	if err != nil {
		return rule{}, err
	}
	return merge(base, own, as, b.schema, strings.ToUpper(method)+" "+path), nil
}

// merge is base's rules with own's applied on top: rules are registered per Go
// type, so a variant carries the merged set rather than inheriting anything.
// Overriding a key the base already rules is legitimate (an endpoint promising
// more than the model), and reported so it is never silent.
func merge(base, own rule, as, schema, endpoint string) rule {
	merged := rule{typ: as, tags: map[string]string{}, wire: map[string]string{}, base: schema, endpoint: endpoint}
	for _, field := range base.order {
		merged.tags[field] = base.tags[field]
		merged.wire[field] = base.wire[field]
		merged.order = append(merged.order, field)
	}
	merged.keys = append(merged.keys, base.keys...)
	for _, field := range own.order {
		if was, override := merged.tags[field]; override {
			fmt.Printf("%s: %s.%s is %q here, not %q\n", as, schema, own.wire[field], own.tags[field], was)
		} else {
			merged.order = append(merged.order, field)
		}
		merged.tags[field] = own.tags[field]
		merged.wire[field] = own.wire[field]
	}
	for _, key := range own.keys {
		if !slices.Contains(merged.keys, key) {
			merged.keys = append(merged.keys, key)
		}
	}
	return merged
}

// fieldForKey is the Go field the given JSON key decodes into.
func fieldForKey(st *ast.StructType, key string) (field string, ok bool) {
	for _, f := range st.Fields.List {
		if f.Tag == nil || len(f.Names) != 1 {
			continue
		}
		if jsonName(strings.Trim(f.Tag.Value, "`")) == key {
			return f.Names[0].Name, true
		}
	}
	return "", false
}

// jsonName is the key a struct tag's `json` entry decodes from.
func jsonName(tag string) string {
	_, value, ok := strings.Cut(tag, `json:"`)
	if !ok {
		return ""
	}
	value, _, _ = strings.Cut(value, `"`)
	name, _, _ := strings.Cut(value, ",")
	return name
}

// cutPresent removes the `present` pseudo-rule from a rule list, reporting
// whether it was there.
func cutPresent(tag string) (rest string, present bool) {
	kept := make([]string, 0, 2)
	for r := range strings.SplitSeq(tag, ",") {
		if strings.TrimSpace(r) == presentRule {
			present = true
			continue
		}
		kept = append(kept, r)
	}
	return strings.Join(kept, ","), present
}

// validationTemplate is the file this command writes.
var validationTemplate = template.Must(template.New("validation").Parse(
	`// Code generated from {{.Client}} and {{.Rules}} by internal/cpapi/gen. DO NOT EDIT.

// Every response type below decodes through Validate, so a body that does not
// carry what this client was generated to expect fails the decode with
// errs.ErrInvalidResponse instead of yielding plausible zero values. See
// validation.go for the mechanism, {{.Rules}} for the rules.

package {{.Package}}

import "encoding/json"

// responseRules is {{.Rules}}, resolved to the generated types.
var responseRules = []responseRule{
{{- range .Rules_}}
	{
		{{- if .Base}}
		// {{.Base}}'s rules, plus what {{.Endpoint}} promises on top.
		{{- end}}
		typ: {{.Type}}{},
		{{- if .Tags}}
		rules: map[string]string{
			{{- range .Tags}}
			{{printf "%q" .Field}}: {{printf "%q" .Tag}},
			{{- end}}
		},
		{{- end}}
		{{- if .Keys}}
		keys: []string{ {{- range $i, $k := .Keys}}{{if $i}}, {{end}}{{printf "%q" $k}}{{end -}} },
		{{- end}}
	},
{{- end}}
}
{{range .Variants}}
// {{.Type}} is a {{.Base}} as answered by {{.Endpoint}}, which promises more
// than the shared model does. Same fields, own identity, so it can carry its
// own rules; convert to {{.Base}} where the difference does not matter.
type {{.Type}} {{.Base}}
{{end}}{{range .Decoders}}
// UnmarshalJSON decodes and validates a {{.}}.
func (d *{{.}}) UnmarshalJSON(data []byte) error {
	type plain {{.}} // shed this method, so the decode below does not recurse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*d = {{.}}(v)
	return Validate(data, d)
}
{{end}}`))

// tag is one field's validator tag, in the order the rules file lists it.
type tag struct {
	Field string
	Tag   string
}

// templateRule is one entry of the generated responseRules table.
type templateRule struct {
	Type     string
	Tags     []tag
	Keys     []string
	Base     string
	Endpoint string
}

func render(pkg, client, rulesFile string, types []string, rules []rule) ([]byte, error) {
	data := struct {
		Package  string
		Client   string
		Rules    string
		Rules_   []templateRule
		Variants []templateRule
		Decoders []string
	}{Package: pkg, Client: client, Rules: rulesFile, Decoders: types}
	for _, r := range rules {
		tr := templateRule{Type: r.typ, Keys: r.keys, Base: r.base, Endpoint: r.endpoint}
		for _, field := range r.order {
			tr.Tags = append(tr.Tags, tag{Field: field, Tag: r.tags[field]})
		}
		data.Rules_ = append(data.Rules_, tr)
		if r.base != "" {
			data.Variants = append(data.Variants, tr)
			data.Decoders = append(data.Decoders, r.typ)
		}
	}

	var b bytes.Buffer
	if err := validationTemplate.Execute(&b, data); err != nil {
		return nil, err
	}
	return format.Source(b.Bytes())
}
