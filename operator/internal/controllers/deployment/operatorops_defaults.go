// What a discovery run needs from its environment, and the defaults for the
// parts a deployment usually does not state.
//
// The probe's image and service account are deployment facts rather than API
// ones: they describe how this operator was installed, not what the run should
// do, so they are not fields on OperatorOps. They live here so that the one
// place they are decided is named, and so a run started on a cluster whose
// chart did not set them fails with a sentence rather than with a pod that
// cannot pull.

package deployment

import (
	"fmt"
	"os"
)

const (
	// NodeProbeImageEnv names the image the probe Jobs run. The chart sets it
	// to the operator's own image, which is where the probe binary ships.
	NodeProbeImageEnv = "SB_NODEPROBE_IMAGE"

	// NodeProbeServiceAccountEnv names the account the probe writes its report
	// as. It is overridable because a deployment may rename it, and defaulted
	// because most do not.
	NodeProbeServiceAccountEnv = "SB_NODEPROBE_SERVICE_ACCOUNT"

	// DefaultNodeProbeServiceAccount is the account the chart creates: a Role
	// with create and update on ConfigMaps in one namespace, and nothing else.
	DefaultNodeProbeServiceAccount = "simplyblock-nodeprobe-sa"
)

// nodeProbeServiceAccount is the account the probe Jobs run as.
func nodeProbeServiceAccount() string {
	if name := os.Getenv(NodeProbeServiceAccountEnv); name != "" {
		return name
	}
	return DefaultNodeProbeServiceAccount
}

// NodeProbeImageError is what a run reports when nothing told the operator
// which image its probes should run.
//
// There is no default worth having. The probe ships in the operator's own
// image, and the operator cannot reliably read its own image reference: a pod's
// spec names whatever tag it was deployed with, which the registry may have
// moved since. So the chart states it, and a deployment that did not gets a
// sentence naming the variable rather than twenty Jobs that cannot pull.
func NodeProbeImageError() error {
	return fmt.Errorf(
		"no probe image: set %s on the operator's deployment to the operator's own image, "+
			"which is where the probe binary ships", NodeProbeImageEnv)
}
