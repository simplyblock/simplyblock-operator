// The rule that the operator reads every kind at the version the API server
// stores, and the scan that holds it.
//
// A kind that has moved to v1alpha2 still serves v1alpha1, but only through the
// conversion webhook, and a fresh install deploys none: an object written at the
// storage version converts nothing, so the webhook exists for the span of an
// upgrade and no longer (design-api-upgrade.md §5). Code that reads the retired
// version therefore reads nothing, and it does so quietly. A LIST at the retired
// version returns an empty list rather than an error, so a controller's cache
// syncs with nothing in it and every cached Get reports NotFound, which reads as
// a missing object rather than as a version that cannot be served.
//
// This lives in its own package because the rule is about the operator as a
// whole rather than about any one controller: a Watch, a Get, an admission
// guard, and a fixture are all the same mistake, and the scan finds them wherever
// they are written.

package storedversion

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// Finding is one reference to a retired version, located for a reader.
type Finding struct {
	File string
	Line int
	Kind string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s is read at v1alpha1, whose reads no fresh install answers", f.File, f.Line, f.Kind)
}

// exemptDirs are the two places the retired version is legitimately named.
//
// The API package defines it and converts it, and the upgrade tool is the one
// client that has a conversion webhook to talk to: it installs one, migrates the
// stored objects, and removes it again.
var exemptDirs = []string{
	"api",
	filepath.Join("internal", "upgrade"),
}

// Scan reports every reference under root to a v1alpha1 type of one of the given
// kinds, outside the exempt directories. Test files are included: a fixture
// seeded at the retired version stands in for an object no cluster hands back,
// which is what let this class of defect pass a green suite.
func Scan(root string, kinds map[string]bool) ([]Finding, error) {
	var findings []Finding

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			for _, exempt := range exemptDirs {
				if rel == exempt {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parsing %s: %w", rel, parseErr)
		}

		// The import alias is read from the file rather than assumed, so a file
		// that spells the v1alpha1 import differently is still caught.
		alias := v1alpha1Alias(file)
		if alias == "" {
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != alias || !kinds[sel.Sel.Name] {
				return true
			}
			findings = append(findings, Finding{
				File: rel,
				Line: fset.Position(sel.Pos()).Line,
				Kind: sel.Sel.Name,
			})
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}

// v1alpha1Alias returns the name a file imports the v1alpha1 API package under,
// or the empty string when it does not import it.
func v1alpha1Alias(file *ast.File) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != "github.com/simplyblock/simplyblock-operator/api/v1alpha1" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "v1alpha1"
	}
	return ""
}
