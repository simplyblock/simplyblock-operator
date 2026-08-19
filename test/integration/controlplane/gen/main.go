// Command gen writes unimplemented.gen.go: a 501 stub for every generated
// ServerInterface method the simulator does not implement itself.
//
// The simulator answers a handful of the control plane's hundred-odd endpoints,
// but it has to satisfy the whole generated interface to be mounted — and going
// through the generated interface is the point. It is what makes the spec, rather
// than a hand-written mux, decide which paths exist:
//
//   - A new endpoint in the spec appears here as a new stub, so regenerating
//     shows it as a diff in a committed file, and a caller that reaches it gets
//     501 rather than 404.
//   - An endpoint that is renamed or removed leaves the handler implementing it
//     matching nothing in the interface, and this fails rather than letting it
//     rot as dead code that no longer serves any route.
//
// Which handlers are hand-written is read off the implementations file, so
// implementing one is nothing more than writing the method: the next generation
// stops stubbing it.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"slices"
	"strings"
)

func main() {
	iface := flag.String("interface", "cpsim.gen.go", "generated server to read ServerInterface from")
	impl := flag.String("impl", "handlers.go", "file whose *Server methods implement it")
	out := flag.String("out", "unimplemented.gen.go", "file to write")
	name := flag.String("name", "ServerInterface", "interface to satisfy")
	flag.Parse()

	fset := token.NewFileSet()

	methods, err := interfaceMethods(fset, *iface, *name)
	if err != nil {
		log.Fatalf("read %s: %v", *iface, err)
	}
	if len(methods) == 0 {
		log.Fatalf("%s: %s has no methods — has the generated server changed shape?", *iface, *name)
	}

	implemented, err := serverMethods(fset, *impl)
	if err != nil {
		log.Fatalf("read %s: %v", *impl, err)
	}

	// A handler that implements nothing is the signal that the spec moved. Left
	// alone it would keep compiling while serving no route at all.
	var orphans []string
	for _, m := range implemented {
		if !slices.ContainsFunc(methods, func(im method) bool { return im.name == m }) {
			orphans = append(orphans, m)
		}
	}
	if len(orphans) > 0 {
		log.Fatalf("%s implements %s, which %s no longer declares — the endpoint was "+
			"renamed or removed in the spec, so the handler has to follow",
			*impl, strings.Join(orphans, ", "), *name)
	}

	var stubs []method
	for _, m := range methods {
		if !slices.Contains(implemented, m.name) {
			stubs = append(stubs, m)
		}
	}

	src, err := render(*iface, *impl, *name, len(methods), implemented, stubs)
	if err != nil {
		log.Fatalf("render: %v", err)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil { //nolint:gosec // generated source
		log.Fatalf("write %s: %v", *out, err)
	}
	fmt.Fprintf(os.Stderr, "%s: %d of %d endpoints implemented, %d stubbed\n",
		*out, len(implemented), len(methods), len(stubs))
}

// method is one interface method: its name and its parameter list as written.
type method struct {
	name   string
	params string
}

// interfaceMethods reads the named interface's methods, rendering each parameter
// list back to source. Rendering rather than reconstructing keeps the stubs
// signature-identical to the interface, including the generated param structs.
func interfaceMethods(fset *token.FileSet, path, name string) ([]method, error) {
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	var out []method
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != name {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return false
		}
		for _, f := range it.Methods.List {
			ft, ok := f.Type.(*ast.FuncType)
			if !ok || len(f.Names) != 1 {
				continue
			}
			m := method{name: f.Names[0].Name}
			var params []string
			for i, p := range ft.Params.List {
				typ, err := render1(fset, p.Type)
				if err != nil {
					continue
				}
				// The first two parameters are the writer and the request; the
				// rest are decoded path and query params a stub ignores.
				switch i {
				case 0:
					params = append(params, "w "+typ)
				case 1:
					params = append(params, "r "+typ)
				default:
					params = append(params, "_ "+typ)
				}
			}
			m.params = strings.Join(params, ", ")
			out = append(out, m)
		}
		return false
	})
	return out, nil
}

// serverMethods lists the methods declared on *Server in path.
func serverMethods(fset *token.FileSet, path string) ([]string, error) {
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
			continue
		}
		star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if id, ok := star.X.(*ast.Ident); ok && id.Name == "Server" {
			out = append(out, fd.Name.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

func render1(fset *token.FileSet, n ast.Node) (string, error) {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, n); err != nil {
		return "", err
	}
	return b.String(), nil
}

func render(ifacePath, implPath, ifaceName string, total int, implemented []string, stubs []method) ([]byte, error) {
	var b bytes.Buffer

	fmt.Fprintf(&b, `// Code generated by ./gen; DO NOT EDIT.
//
// A 501 stub for each of %s's %d endpoints that %s does not implement.
// %d are implemented; the rest answer "not implemented", which is a truthful
// answer and a distinguishable one — a 404 from here would look like a missing
// volume rather than a missing simulator.
//
// A new stub in this file means the control-plane spec grew an endpoint.

package cpsim

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
)

// The simulator satisfies the whole generated interface.
var _ %s = (*Server)(nil)

`, ifacePath, total, implPath, len(implemented), ifaceName)

	fmt.Fprintf(&b, "// Implemented in %s:\n", implPath)
	for _, m := range implemented {
		fmt.Fprintf(&b, "//   %s\n", m)
	}
	b.WriteString("\n")

	for _, m := range stubs {
		fmt.Fprintf(&b, "func (s *Server) %s(%s) {\n\tnotImplemented(w, r)\n}\n\n", m.name, m.params)
	}

	// openapi_types is only referenced by stubs that take a UUID parameter, and
	// a spec where none did would leave the import unused.
	src := b.Bytes()
	if !bytes.Contains(src, []byte("openapi_types.")) {
		src = bytes.Replace(src, []byte("\n\topenapi_types \"github.com/oapi-codegen/runtime/types\"\n"), []byte(""), 1)
	}
	return format.Source(src)
}
