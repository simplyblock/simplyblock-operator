package autoplacement

import (
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

func GetConfig(spec *simplyblockv1alpha2.VolumeAutoPlacementSettings) simplyblockv1alpha2.VolumeAutoPlacementSettings {
	return ptr.From(spec, simplyblockv1alpha2.VolumeAutoPlacementSettings{
		PrometheusURL: ptr.To(utils.DefaultPrometheusURL),
	})
}
