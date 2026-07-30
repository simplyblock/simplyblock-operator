package volumemigration

import (
	"os"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

const DefaultRebalancerImage = "quay.io/simplyblock-io/simplyblock-rebalancer:latest"

// RebalancerImageEnvVar is the environment variable used to override the
// default rebalancer image. The Helm chart sets this during installation to
// the same tag as the operator so the rebalancer and operator stay in lockstep.
const RebalancerImageEnvVar = "SIMPLYBLOCK_REBALANCER_IMAGE"

// defaultRebalancerImage returns the rebalancer image from the
// SIMPLYBLOCK_REBALANCER_IMAGE environment variable, falling back to
// DefaultRebalancerImage when it is unset or empty.
func defaultRebalancerImage() string {
	if image := os.Getenv(RebalancerImageEnvVar); image != "" {
		return image
	}
	return DefaultRebalancerImage
}

func GetConfig(spec *simplyblockv1alpha1.VolumeMigrationSettings) simplyblockv1alpha1.VolumeMigrationSettings {
	return ptr.From(spec, simplyblockv1alpha1.VolumeMigrationSettings{
		RebalancerImage: ptr.To(defaultRebalancerImage()),
	})
}
