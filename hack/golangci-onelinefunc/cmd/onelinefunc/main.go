// Runs the onelinefunc rule on its own, without golangci-lint.
//
// This is the front end a Makefile target or a CI step uses:
//
//	go run ./hack/golangci-onelinefunc/cmd/onelinefunc ./...
//
// It exists so the rule can be enforced without rebuilding golangci-lint, and
// so it can cover test/integration, which repo_lint.yaml's matrix does not.
package main

import (
	"github.com/simplyblock/golangci-onelinefunc"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(onelinefunc.Analyzer)
}
