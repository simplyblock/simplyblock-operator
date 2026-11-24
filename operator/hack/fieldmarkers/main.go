// The field-marker generator: resolves the +simplyblock:operatorManaged and
// +simplyblock:invisible field markers in the API packages to field paths, and
// prints the CRD annotations that carry them, for `hack/apply-field-markers.sh`
// to write after controller-gen. It lives in hack/ because it is build tooling
// for this module's manifests and ships in no binary.
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

package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
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
)

// annotationKeys names the CRD annotation each marker is written to.
var annotationKeys = map[string]string{
	markerOperatorManaged: "simplyblock.io/operator-managed-fields",
	markerInvisible:       "simplyblock.io/invisible-fields",
}

// Fields maps a marker to the paths it marks, per API version.
type Fields map[string]map[string][]string

// Kinds maps "<group>/<Kind>" to the marked fields of that kind.
type Kinds map[string]Fields

var (
	markerLine = regexp.MustCompile(`^\+simplyblock:(\S+)`)
	groupName  = regexp.MustCompile(`(?m)^//\s*\+groupName=(\S+)`)
)

// root is a kind: a type carrying +kubebuilder:object:root=true in a package
// that declares a +groupName.
type root struct {
	group, version string
	named          *types.Named
}

// collect loads the packages matching patterns, relative to dir, and resolves
// the markers of every root kind in them.
func collect(dir string, patterns ...string) (Kinds, error) {
	cfg := &packages.Config{
		Dir:  dir,
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load %v: %w", patterns, err)
	}
	// Field markers are looked up by the position of the field's declaration,
	// across every loaded package, since a kind may embed a type from another.
	marks := map[token.Pos][]string{}
	var roots []root
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			return nil, fmt.Errorf("load %s: %v", pkg.PkgPath, pkg.Errors[0])
		}
		for _, file := range pkg.Syntax {
			if err := fieldMarkers(pkg.Fset, file, marks); err != nil {
				return nil, err
			}
		}
		roots = append(roots, rootsOf(pkg)...)
	}
	kinds := Kinds{}
	for _, r := range roots {
		found := map[string]map[string]bool{}
		walk(r.named, "", r.named.Obj().Pkg().Path(), marks, map[*types.Named]bool{}, found)
		if len(found) > 0 {
			kinds.add(r, found)
		}
	}
	return kinds, nil
}

// fieldMarkers records the +simplyblock: markers of every field in file, keyed
// by the position of the field's name (or type, for an embedded field), and
// refuses a marker it does not know.
func fieldMarkers(fset *token.FileSet, file *ast.File, marks map[token.Pos][]string) error {
	var err error
	ast.Inspect(file, func(n ast.Node) bool {
		field, ok := n.(*ast.Field)
		if !ok || field.Doc == nil || err != nil {
			return err == nil
		}
		var found []string
		for _, c := range field.Doc.List {
			m := markerLine.FindStringSubmatch(strings.TrimSpace(strings.TrimPrefix(c.Text, "//")))
			if m == nil {
				continue
			}
			if _, known := annotationKeys[m[1]]; !known {
				err = fmt.Errorf("%s: unknown marker +simplyblock:%s (known: +simplyblock:%s, +simplyblock:%s)",
					fset.Position(c.Pos()), m[1], markerOperatorManaged, markerInvisible)
				return false
			}
			found = append(found, m[1])
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

// walk records the marked fields below t, whose path is prefix. It stays inside
// the module that declares the kind (modulePrefix) and stops at a type already
// on the way down, which ends a recursive type after one level.
func walk(t types.Type, prefix, modulePrefix string, marks map[token.Pos][]string,
	onPath map[*types.Named]bool, found map[string]map[string]bool) {
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
		f := st.Field(i)
		name, inline := jsonName(f, st.Tag(i))
		if name == "-" {
			continue
		}
		path := prefix
		if !inline {
			path = join(prefix, name)
			for _, m := range marks[f.Pos()] {
				if found[m] == nil {
					found[m] = map[string]bool{}
				}
				found[m][path] = true
			}
		}
		walk(f.Type(), path, modulePrefix, marks, onPath, found)
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
			Kind string `json:"kind"`
		} `json:"names"`
	} `json:"spec"`
}

func run(crdDir string) error {
	kinds, err := collect(".", "./api/...")
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
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fieldmarkers <crd-bases-dir>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "fieldmarkers:", err)
		os.Exit(1)
	}
}
