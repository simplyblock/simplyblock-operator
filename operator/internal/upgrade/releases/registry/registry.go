// The list of release modules the shipped tool knows, in release order. It is a
// package of its own because every module imports the releases package for its
// types, and the list imports every module: kept in releases, the two would
// import each other. Adding what a new release needs is a new module package
// and one line here.

package registry

import (
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/releases"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/releases/r26_4_0_beta7"
)

// Modules returns every release module, configured with the run's options.
func Modules(o releases.Options) []releases.Module {
	return []releases.Module{
		r26_4_0_beta7.Module(o),
	}
}
