// The rule: a function declaration's body does not share a line with its
// signature.
//
// No existing linter expresses it. revive has no such check among its rules,
// and neither gofmt nor gofumpt reformats `func f() int { return 1 }`, both of
// which were verified rather than assumed. So it is written here once and used
// from two front ends: golangci-lint loads it as a module plugin, and
// cmd/onelinefunc runs the same analyzer on its own.
package onelinefunc

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"
)

// Analyzer reports every function declaration whose body opens and closes on
// one line, provided the body holds at least one statement.
//
// Two things are deliberately not reported. An empty body is idiomatic, because
// `func (noopReporter) Report() {}` is how a no-op implementation of an
// interface is written and spreading it over three lines says less. And a
// function literal is not a declaration: a closure passed to a Ginkgo `It`, or
// a `defer func() { cancel() }()`, reads better on one line, which is why this
// is about declarations only.
var Analyzer = &analysis.Analyzer{
	Name: "onelinefunc",
	Doc:  "a function declaration's body does not share a line with its signature",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || len(fn.Body.List) == 0 {
				continue
			}

			open := pass.Fset.Position(fn.Body.Lbrace)
			closing := pass.Fset.Position(fn.Body.Rbrace)
			if open.Line != closing.Line {
				continue
			}

			pass.Report(analysis.Diagnostic{
				Pos:      fn.Body.Lbrace,
				End:      fn.Body.Rbrace,
				Category: "onelinefunc",
				Message:  "function body shares a line with its signature, put it on its own lines",
			})
		}
	}

	return nil, nil
}
