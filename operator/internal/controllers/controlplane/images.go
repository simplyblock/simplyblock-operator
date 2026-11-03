// The mirror registry the control-plane install's own images are pulled from on
// an air-gapped cluster. It is set once at setup, from the operator's
// environment, so the workload builders stay functions of the ControlPlane.

package controlplane

import (
	"sync/atomic"

	"github.com/simplyblock/simplyblock-operator/internal/prerequisites"
)

var imageMirrorRegistry atomic.Value

// SetImageMirrorRegistry sets the registry image resolves against. Empty keeps
// the upstream registries.
func SetImageMirrorRegistry(registry string) { imageMirrorRegistry.Store(registry) }

// image is ref, moved to the mirror registry when one is set.
func image(ref string) string {
	registry, _ := imageMirrorRegistry.Load().(string)
	return prerequisites.Mirror(ref, registry)
}
