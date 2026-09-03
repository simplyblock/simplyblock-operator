package autoplacement

import (
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func GetConfig(spec *simplyblockv1alpha1.VolumeAutoPlacementSettings) simplyblockv1alpha1.VolumeAutoPlacementSettings {
	return ptr.From(spec, simplyblockv1alpha1.VolumeAutoPlacementSettings{
		PrometheusURL: ptr.To(utils.DefaultPrometheusURL),
	})
}
