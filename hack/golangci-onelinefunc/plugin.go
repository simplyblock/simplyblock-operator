// Registers the onelinefunc rule as a golangci-lint module plugin.
//
// This package needs a go.mod of its own because golangci-lint compiles a
// plugin into a bespoke binary, and nothing in this repository imports it. The
// house rule against new modules is about shared code the operator and the CSI
// driver would each need a replace directive for; a build-time tool neither of
// them links is not that.
package onelinefunc

import (
	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
)

func init() {
	register.Plugin("onelinefunc", New)
}

// Plugin carries no settings. The rule has nothing to configure: a body either
// shares its signature's line or it does not.
type Plugin struct{}

// New builds the plugin. golangci-lint hands over the `settings` block from
// .golangci.yml, and this rule reads none of it.
func New(_ any) (register.LinterPlugin, error) {
	return &Plugin{}, nil
}

// BuildAnalyzers returns the single analyzer this plugin provides.
func (p *Plugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{Analyzer}, nil
}

// GetLoadMode asks for syntax only. The rule reads brace positions and needs no
// type information.
func (p *Plugin) GetLoadMode() string {
	return register.LoadModeSyntax
}
