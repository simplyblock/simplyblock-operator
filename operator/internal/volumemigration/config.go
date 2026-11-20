// The image the rebalancer runs, and the migration settings a cluster gets when
// it states none.
//
// One resolver answers for every caller: the migration Jobs, the latency
// benchmark, and the injected rebalancer. They used to carry two different
// literal defaults, and neither was the operator's own release.

package volumemigration

import (
	"fmt"
	"os"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/ownimage"
)

// RebalancerImageEnvVar names the rebalancer image a deployment pins. The chart
// sets it to the operator's tag unless told otherwise, so it is the way to run
// a rebalancer from a different tag than the operator.
const RebalancerImageEnvVar = "SIMPLYBLOCK_REBALANCER_IMAGE"

// rebalancerRepository is the repository the rebalancer ships in, beside the
// operator's in the same registry and under the same tag.
const rebalancerRepository = "simplyblock-rebalancer"

// RebalancerImage is the image a rebalancer runs for a cluster with these
// settings: the cluster's own pin, then the deployment's, then the rebalancer
// of the operator's own release.
//
// There is no literal fallback. A Job that cannot be given the operator's
// release fails with a sentence naming what to set, rather than running a
// binary from some other release against the host's fabric.
func RebalancerImage(spec *simplyblockv1alpha2.VolumeMigrationSettings) (string, error) {
	if spec != nil {
		if image := ptr.From(spec.RebalancerImage, ""); image != "" {
			return image, nil
		}
	}
	if image := os.Getenv(RebalancerImageEnvVar); image != "" {
		return image, nil
	}
	image, err := ownimage.SiblingOfOperator(rebalancerRepository)
	if err != nil {
		return "", fmt.Errorf(
			"no rebalancer image: set spec.volumeMigrationSettings.rebalancerImage on the "+
				"StorageCluster, %s on the operator's deployment, or %s to the operator's "+
				"own image (%w)",
			RebalancerImageEnvVar, ownimage.Env, err)
	}
	return image, nil
}

// GetConfig is the cluster's migration settings with the rebalancer image
// resolved where the cluster pins none.
//
// An image that cannot be resolved is left unset, which the latency benchmark
// and the rebalancer injector both read as the rebalancer not being configured.
func GetConfig(spec *simplyblockv1alpha2.VolumeMigrationSettings) simplyblockv1alpha2.VolumeMigrationSettings {
	settings := ptr.From(spec, simplyblockv1alpha2.VolumeMigrationSettings{})
	if image, err := RebalancerImage(spec); err == nil {
		settings.RebalancerImage = ptr.To(image)
	}
	return settings
}
