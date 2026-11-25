// The field-marker generator: resolves the +simplyblock:operatorManaged,
// +simplyblock:advanced, and +simplyblock:invisible field markers in the API
// packages to field paths, and prints the CRD annotations that carry them, for
// `hack/apply-field-markers.sh` to write after controller-gen. It lives in
// hack/ because it is build tooling for this module's manifests and ships in
// no binary.
//
// controller-gen has no way to carry a custom field marker into a CRD: a
// vendor key in the schema is dropped by the API server, which knows only the
// keys of JSONSchemaProps. An annotation on the CRD object survives, so the
// Control Center reads the markers from there (control-center/cr-view.jsx).
//
// Usage: go run ./hack/fieldmarkers <crd-bases-dir>
//
// prints one line per annotation, <manifest>\t<key>\t<value>, where the value
// is JSON keyed by API version: {"v1alpha2":["spec.a","status.b"]}. A path
// descends through structs, pointers, slices, and maps the way the CRD schema
// does: the elements of a list or map add no segment of their own.
//
// Usage: go run ./hack/fieldmarkers -webhook <file.go> <crd-bases-dir>
//
// writes the Go file the operator-managed-fields validator is built from
// instead: the spec paths of every +simplyblock:operatorManaged field per kind,
// with the writers each admits, and a +kubebuilder:webhook marker per kind for
// controller-gen to register. There a path keeps one segment per JSON level and
// writes "*" for the values of a map, because a projection over unstructured
// JSON cannot tell a map from a struct by looking.
//
// +simplyblock:operatorManaged takes one optional argument, writers=<a>;<b>,
// naming identities besides the operator's own that may write the field. The
// known writers are the keys of knownWriters.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"sigs.k8s.io/yaml"
)

const (
	markerOperatorManaged = "operatorManaged"
	markerInvisible       = "invisible"
	markerAdvanced        = "advanced"
)

// annotationKeys names the CRD annotation each marker is written to.
var annotationKeys = map[string]string{
	markerOperatorManaged: "simplyblock.io/operator-managed-fields",
	markerInvisible:       "simplyblock.io/invisible-fields",
	markerAdvanced:        "simplyblock.io/advanced-fields",
}

// knownWriters are the identities a writers= argument may name, besides the
// operator, which may always write an operator-managed field. The operator maps
// each to the usernames it stands for at run time.
var knownWriters = map[string]bool{
	// hub is the work agent that applies the hub's ManifestWorks on a site,
	// which delivers a stretched site's ClusterDeploymentConfig.
	"hub": true,
	// `csi` is the CSI controller, whose service account is named after the
	// SimplyblockDriver, so the operator resolves it at request time.
	"csi": true,
}

// Fields maps a marker to the paths it marks, per API version.
type Fields map[string]map[string][]string

// Kinds maps "<group>/<Kind>" to the marked fields of that kind.
type Kinds map[string]Fields

// ManagedField is one operator-managed field as the webhook table carries it.
type ManagedField struct {
	// Path is the field's path as the annotations spell it.
	Path string
	// Segments is the path one JSON level at a time, "*" for a map's values.
	Segments []string
	// Writers are the identities besides the operator that may write it.
	Writers []string
}

// Managed maps "<group>/<Kind>" to its operator-managed fields per API version.
type Managed map[string]map[string][]ManagedField

// mark is one +simplyblock: marker on a field, with its writers when it is
// operatorManaged.
type mark struct {
	name    string
	writers []string
}

var (
	markerLine = regexp.MustCompile(`^\+simplyblock:(\S+)`)
	groupName  = regexp.MustCompile(`(?m)^//\s*\+groupName=(\S+)`)
)

// parseMarker splits the text after +simplyblock: into the marker and its
// arguments, and refuses a marker or a writer it does not know.
func parseMarker(text string) (mark, error) {
	name, args, hasArgs := strings.Cut(text, ":")
	if _, known := annotationKeys[name]; !known {
		return mark{}, fmt.Errorf("unknown marker +simplyblock:%s (known: +simplyblock:%s, +simplyblock:%s, +simplyblock:%s)",
			name, markerOperatorManaged, markerInvisible, markerAdvanced)
	}
	if !hasArgs {
		return mark{name: name}, nil
	}
	if name != markerOperatorManaged {
		return mark{}, fmt.Errorf("+simplyblock:%s takes no arguments", name)
	}
	list, ok := strings.CutPrefix(args, "writers=")
	if !ok || list == "" {
		return mark{}, fmt.Errorf("+simplyblock:%s takes one argument, writers=<name>;<name>", name)
	}
	m := mark{name: name}
	for _, w := range strings.Split(list, ";") {
		if !knownWriters[w] {
			return mark{}, fmt.Errorf("+simplyblock:%s names writer %q, which is not one of %v",
				name, w, sortedKeys(knownWriters))
		}
		m.writers = append(m.writers, w)
	}
	sort.Strings(m.writers)
	return m, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// found is what the walk below one kind records.
type found struct {
	// paths maps a marker to the annotation paths it marks.
	paths map[string]map[string]bool
	// managed maps an operator-managed field's annotation path to its entry.
	managed map[string]ManagedField
}

// root is a kind: a type carrying +kubebuilder:object:root=true in a package
// that declares a +groupName.
type root struct {
	group, version string
	named          *types.Named
}

// collect loads the packages matching patterns, relative to the working
// directory, and resolves the markers of every root kind in them: as the paths
// the annotations carry, and as the table the webhook is built from.
func collect(patterns ...string) (Kinds, Managed, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo |
			// from source, not export data: building export data fails outright on a
			// type error, where the type checker reports it and carries on
			packages.NeedImports | packages.NeedDeps,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, nil, fmt.Errorf("load %v: %w", patterns, err)
	}
	// Field markers are looked up by the position of the field's declaration,
	// across every loaded package, since a kind may embed a type from another.
	marks := map[token.Pos][]mark{}
	var roots []root
	for _, pkg := range pkgs {
		// A type error does not stop the walk: make manifests runs before make
		// generate, so a deepcopy file that still names a removed field is the
		// normal state of an API change, and the struct types resolve anyway.
		// A package that cannot be listed or parsed has no types to walk.
		for _, e := range pkg.Errors {
			if e.Kind != packages.TypeError {
				return nil, nil, fmt.Errorf("load %s: %v", pkg.PkgPath, e)
			}
		}
		for _, file := range pkg.Syntax {
			if err := fieldMarkers(pkg.Fset, file, marks); err != nil {
				return nil, nil, err
			}
		}
		roots = append(roots, rootsOf(pkg)...)
	}
	kinds, managed := Kinds{}, Managed{}
	for _, r := range roots {
		f := found{paths: map[string]map[string]bool{}, managed: map[string]ManagedField{}}
		walk(r.named, "", nil, r.named.Obj().Pkg().Path(), marks, map[*types.Named]bool{}, f)
		if len(f.paths) > 0 {
			kinds.add(r, f.paths)
		}
		if len(f.managed) > 0 {
			managed.add(r, f.managed)
		}
	}
	return kinds, managed, nil
}

// fieldMarkers records the +simplyblock: markers of every field in file, keyed
// by the position of the field's name (or type, for an embedded field), and
// refuses a marker or a writer it does not know.
func fieldMarkers(fset *token.FileSet, file *ast.File, marks map[token.Pos][]mark) error {
	var err error
	ast.Inspect(file, func(n ast.Node) bool {
		field, ok := n.(*ast.Field)
		if !ok || field.Doc == nil || err != nil {
			return err == nil
		}
		var found []mark
		for _, c := range field.Doc.List {
			m := markerLine.FindStringSubmatch(strings.TrimSpace(strings.TrimPrefix(c.Text, "//")))
			if m == nil {
				continue
			}
			parsed, perr := parseMarker(m[1])
			if perr != nil {
				err = fmt.Errorf("%s: %w", fset.Position(c.Pos()), perr)
				return false
			}
			found = append(found, parsed)
		}
		if len(found) > 0 {
			pos := field.Type.Pos()
			if len(field.Names) > 0 {
				pos = field.Names[0].Pos()
			}
			marks[pos] = found
		}
		return true
	})
	return err
}

// rootsOf lists the kinds a package declares, none when it has no +groupName.
func rootsOf(pkg *packages.Package) []root {
	group := ""
	for _, file := range pkg.Syntax {
		for _, cg := range file.Comments {
			if m := groupName.FindStringSubmatch(rawText(cg)); m != nil {
				group = m[1]
			}
		}
	}
	if group == "" {
		return nil
	}
	var roots []root
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				if !strings.Contains(typeComments(pkg.Fset, file, gen, ts), "+kubebuilder:object:root=true") ||
					strings.HasSuffix(ts.Name.Name, "List") {
					continue
				}
				if named, ok := pkg.TypesInfo.Defs[ts.Name].Type().(*types.Named); ok {
					roots = append(roots, root{group: group, version: pkg.Name, named: named})
				}
			}
		}
	}
	return roots
}

// add records a kind's marked paths, sorted, under its group and version.
func (k Kinds) add(r root, found map[string]map[string]bool) {
	key := r.group + "/" + r.named.Obj().Name()
	if k[key] == nil {
		k[key] = Fields{}
	}
	for marker, paths := range found {
		if k[key][marker] == nil {
			k[key][marker] = map[string][]string{}
		}
		list := make([]string, 0, len(paths))
		for p := range paths {
			list = append(list, p)
		}
		sort.Strings(list)
		k[key][marker][r.version] = list
	}
}

// add records a kind's operator-managed fields, sorted by path, under its group
// and version.
func (m Managed) add(r root, fields map[string]ManagedField) {
	key := r.group + "/" + r.named.Obj().Name()
	if m[key] == nil {
		m[key] = map[string][]ManagedField{}
	}
	list := make([]ManagedField, 0, len(fields))
	for _, f := range fields {
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	m[key][r.version] = list
}

// typeComments is the text a type's markers can sit in, as controller-gen reads
// it: the doc comment, and the comment block one blank line above it (or above
// the declaration, when there is no doc comment).
func typeComments(fset *token.FileSet, file *ast.File, gen *ast.GenDecl, ts *ast.TypeSpec) string {
	doc := ts.Doc
	if doc == nil {
		doc = gen.Doc
	}
	text, top := "", fset.Position(gen.Pos()).Line
	if doc != nil {
		text, top = rawText(doc), fset.Position(doc.Pos()).Line
	}
	for _, cg := range file.Comments {
		if fset.Position(cg.End()).Line == top-2 {
			text += rawText(cg)
		}
	}
	return text
}

func rawText(cg *ast.CommentGroup) string {
	var b strings.Builder
	for _, c := range cg.List {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// walk records the marked fields below t, whose annotation path is prefix and
// whose JSON segments are segs. It stays inside the module that declares the
// kind (modulePrefix) and stops at a type already on the way down, which ends a
// recursive type after one level.
func walk(t types.Type, prefix string, segs []string, modulePrefix string, marks map[token.Pos][]mark,
	onPath map[*types.Named]bool, f found) {
	for {
		switch u := t.(type) {
		case *types.Pointer:
			t = u.Elem()
			continue
		case *types.Slice:
			t = u.Elem()
			continue
		case *types.Array:
			t = u.Elem()
			continue
		case *types.Map:
			t = u.Elem()
			segs = append(segs[:len(segs):len(segs)], "*")
			continue
		}
		break
	}
	named, ok := t.(*types.Named)
	if !ok {
		return
	}
	if pkg := named.Obj().Pkg(); pkg == nil || !sameModule(pkg.Path(), modulePrefix) || onPath[named] {
		return
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return
	}
	onPath[named] = true
	defer delete(onPath, named)
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		name, inline := jsonName(field, st.Tag(i))
		if name == "-" {
			continue
		}
		path, fieldSegs := prefix, segs
		if !inline {
			path = join(prefix, name)
			fieldSegs = append(segs[:len(segs):len(segs)], name)
			for _, m := range marks[field.Pos()] {
				if f.paths[m.name] == nil {
					f.paths[m.name] = map[string]bool{}
				}
				f.paths[m.name][path] = true
				if m.name == markerOperatorManaged {
					f.managed[path] = ManagedField{Path: path, Segments: fieldSegs, Writers: m.writers}
				}
			}
		}
		walk(field.Type(), path, fieldSegs, modulePrefix, marks, onPath, f)
	}
}

// sameModule reports whether two package paths share their first three
// elements, github.com/<org>/<repo>, which is the module for every package
// this repository declares kinds in. It keeps the walk out of apimachinery.
func sameModule(a, b string) bool {
	head := func(p string) string {
		parts := strings.SplitN(p, "/", 4)
		if len(parts) > 3 {
			parts = parts[:3]
		}
		return strings.Join(parts, "/")
	}
	return head(a) == head(b)
}

// jsonName is the field's serialized name, and whether it is inlined into its
// parent: through the inline option of its tag, or as an embedded field with
// no name of its own.
func jsonName(f *types.Var, tag string) (string, bool) {
	value := reflect.StructTag(tag).Get("json")
	name, opts, _ := strings.Cut(value, ",")
	if strings.Contains(","+opts+",", ",inline,") || (f.Embedded() && name == "") {
		return "", true
	}
	if name == "" {
		name = f.Name()
	}
	return name, false
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// annotations renders a kind's marked fields as its CRD annotations.
func annotations(f Fields) map[string]string {
	out := map[string]string{}
	for marker, byVersion := range f {
		b, err := json.Marshal(byVersion) // map keys are sorted, so the value is stable
		if err != nil {
			panic(err)
		}
		out[annotationKeys[marker]] = string(b)
	}
	return out
}

// crdIdentity is the part of a CRD manifest the output is keyed on.
type crdIdentity struct {
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind   string `json:"kind"`
			Plural string `json:"plural"`
		} `json:"names"`
	} `json:"spec"`
}

// webhookPath is where the operator serves the operator-managed-fields
// validator, for every kind in the table.
const webhookPath = "/validate-operator-managed-fields"

// renderWebhook writes the Go file the validator is built from, in package pkg:
// the kinds that have operator-managed spec fields, each with a webhook marker
// and its table entry. Status paths are left out, because status is written
// through its own subresource, which the webhooks are not registered for.
func renderWebhook(managed Managed, plurals map[string]string, pkg string) ([]byte, error) {
	keys := make([]string, 0, len(managed))
	for k := range managed {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var markers, table bytes.Buffer
	for _, key := range keys {
		group, kind, _ := strings.Cut(key, "/")
		versions := make([]string, 0, len(managed[key]))
		for v := range managed[key] {
			versions = append(versions, v)
		}
		sort.Strings(versions)
		var served []string
		for _, version := range versions {
			var entries []string
			for _, f := range managed[key][version] {
				if f.Segments[0] != "spec" {
					continue
				}
				entry := fmt.Sprintf("{Path: %q, Segments: %s", f.Path, goStrings(f.Segments))
				if len(f.Writers) > 0 {
					entry += ", Writers: " + goStrings(f.Writers)
				}
				entries = append(entries, entry+"},")
			}
			if len(entries) == 0 {
				continue
			}
			served = append(served, version)
			fmt.Fprintf(&table, "\t{Group: %q, Version: %q, Kind: %q}: {\n\t\t%s\n\t},\n",
				group, version, kind, strings.Join(entries, "\n\t\t"))
		}
		if len(served) == 0 {
			continue
		}
		plural, ok := plurals[key]
		if !ok {
			return nil, fmt.Errorf("kind %s has operator-managed fields but no CRD manifest", key)
		}
		fmt.Fprintf(&markers,
			"// +kubebuilder:webhook:path=%s,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,sideEffects=None,"+
				"groups=%s,resources=%s,verbs=update,versions=%s,name=voperatormanaged-%s.%s,admissionReviewVersions=v1\n",
			webhookPath, group, plural, strings.Join(served, ";"), plural, group)
	}

	src := fmt.Sprintf(`// Code generated by hack/fieldmarkers. DO NOT EDIT.

// The fields marked +simplyblock:operatorManaged, per kind and version, and the
// webhook registration for every kind that has one in its spec. The
// operator-managed-fields validator refuses an update that changes one of them
// unless it comes from the operator or from a writer the field admits. It is
// generated from the API types by hack/fieldmarkers, so a marker is the one
// place a field is declared operator-managed.

package %s

import "k8s.io/apimachinery/pkg/runtime/schema"

%s
// generatedOperatorManagedFields is the table the validator reads.
var generatedOperatorManagedFields = map[schema.GroupVersionKind][]OperatorManagedField{
%s}
`, pkg, markers.String(), table.String())
	return format.Source([]byte(src))
}

func goStrings(list []string) string {
	quoted := make([]string, len(list))
	for i, s := range list {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return "[]string{" + strings.Join(quoted, ", ") + "}"
}

// readCRDs maps "<group>/<Kind>" to the manifest and the plural of every CRD in
// crdDir.
func readCRDs(crdDir string) (map[string]string, map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		return nil, nil, err
	}
	manifests, plurals := map[string]string{}, map[string]string{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, err
		}
		var crd crdIdentity
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", file, err)
		}
		key := crd.Spec.Group + "/" + crd.Spec.Names.Kind
		manifests[key], plurals[key] = file, crd.Spec.Names.Plural
	}
	return manifests, plurals, nil
}

func writeWebhook(out, crdDir string) error {
	_, managed, err := collect("./api/...")
	if err != nil {
		return err
	}
	_, plurals, err := readCRDs(crdDir)
	if err != nil {
		return err
	}
	src, err := renderWebhook(managed, plurals, filepath.Base(filepath.Dir(out)))
	if err != nil {
		return err
	}
	return os.WriteFile(out, src, 0o644)
}

func run(crdDir string) error {
	kinds, _, err := collect("./api/...")
	if err != nil {
		return err
	}
	manifests, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		return err
	}
	matched := map[string]bool{}
	for _, manifest := range manifests {
		raw, err := os.ReadFile(manifest)
		if err != nil {
			return err
		}
		var crd crdIdentity
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return fmt.Errorf("%s: %w", manifest, err)
		}
		key := crd.Spec.Group + "/" + crd.Spec.Names.Kind
		fields, ok := kinds[key]
		if !ok {
			continue
		}
		matched[key] = true
		ann := annotations(fields)
		names := make([]string, 0, len(ann))
		for k := range ann {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Printf("%s\t%s\t%s\n", manifest, k, ann[k])
		}
	}
	for key := range kinds {
		if !matched[key] {
			return fmt.Errorf("kind %s carries field markers but has no CRD manifest in %s", key, crdDir)
		}
	}
	return nil
}

func main() {
	webhookOut := flag.String("webhook", "",
		"write the operator-managed-fields table to this Go file instead of printing annotations")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: fieldmarkers [-webhook <file.go>] <crd-bases-dir>")
		os.Exit(2)
	}
	generate := run
	if *webhookOut != "" {
		generate = func(crdDir string) error { return writeWebhook(*webhookOut, crdDir) }
	}
	if err := generate(flag.Arg(0)); err != nil {
		fmt.Fprintln(os.Stderr, "fieldmarkers:", err)
		os.Exit(1)
	}
}
