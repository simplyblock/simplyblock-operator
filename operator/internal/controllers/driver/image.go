// Which image the two plugins run when the spec does not say.
//
// The driver ships from the same build as the operator that reconciles it, so
// the default is the operator's own registry and tag with the CSI driver's
// repository in place of the operator's. A deployment that states nothing
// therefore runs the driver belonging to the operator it is running under,
// which is the pairing every release is tested as.
//
// The operator's image comes from the environment rather than from its own pod.
// A pod's spec names whatever tag it was deployed with and a registry may have
// moved that tag since, which is the same reason the probe Jobs read theirs
// from SB_NODEPROBE_IMAGE rather than looking themselves up.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §3.1.

package driver

import (
	"fmt"
	"os"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/ownimage"
)

const (
	// OperatorImageEnv names the operator's own image. The chart sets it to
	// exactly what it deploys the manager from.
	OperatorImageEnv = ownimage.Env

	// csiDriverRepository is the repository the driver ships in, beside the
	// operator's in the same registry.
	csiDriverRepository = "spdkcsi"
)

// driverImage is the image both plugins run: the spec's where it states one,
// and the operator's own registry and tag with the CSI repository otherwise.
func driverImage(d *simplyblockv1alpha2.SimplyblockDriver) (string, error) {
	if d.Spec.Image != "" {
		return d.Spec.Image, nil
	}

	operator := os.Getenv(OperatorImageEnv)
	if operator == "" {
		return "", fmt.Errorf(
			"spec.image is unset and nothing says which image to default to: "+
				"set it on the object, or set %s on the operator's deployment to the "+
				"operator's own image, which is where the default's registry and tag come from",
			OperatorImageEnv)
	}

	derived, err := ownimage.Sibling(operator, csiDriverRepository)
	if err != nil {
		return "", fmt.Errorf(
			"spec.image is unset and the operator's own image %q cannot be turned into a "+
				"driver image: %w; set spec.image on the object instead",
			operator, err)
	}
	return derived, nil
}
