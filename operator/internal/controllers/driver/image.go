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
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// OperatorImageEnv names the operator's own image. The chart sets it to
	// exactly what it deploys the manager from.
	OperatorImageEnv = "SB_OPERATOR_IMAGE"

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

	derived, err := siblingImage(operator, csiDriverRepository)
	if err != nil {
		return "", fmt.Errorf(
			"spec.image is unset and the operator's own image %q cannot be turned into a "+
				"driver image: %w; set spec.image on the object instead",
			operator, err)
	}
	return derived, nil
}

// siblingImage rewrites a reference to name a different repository in the same
// registry at the same tag.
//
// A digest is dropped rather than carried, because a digest identifies one
// image and says nothing about another. A reference that carries only a digest
// has no tag to reuse and is refused, since guessing one would deploy something
// nobody named.
func siblingImage(reference, repository string) (string, error) {
	withoutDigest, _, _ := strings.Cut(reference, "@")

	// The tag is what follows the last colon, and only when that colon comes
	// after the last slash. A registry may carry a port, and `host:5000/repo`
	// has a colon that is not a tag separator.
	slash := strings.LastIndex(withoutDigest, "/")
	colon := strings.LastIndex(withoutDigest, ":")
	if colon < 0 || colon < slash {
		return "", fmt.Errorf("it names no tag")
	}
	base, tag := withoutDigest[:colon], withoutDigest[colon+1:]
	if tag == "" {
		return "", fmt.Errorf("it names no tag")
	}
	if slash < 0 {
		return "", fmt.Errorf("it names no registry")
	}

	return base[:slash+1] + repository + ":" + tag, nil
}
