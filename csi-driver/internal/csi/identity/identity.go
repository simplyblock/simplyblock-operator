package identity

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

type Server struct {
	*csicommon.DefaultIdentityServer
}

func New(d *csicommon.CSIDriver) *Server {
	return &Server{
		DefaultIdentityServer: csicommon.NewDefaultIdentityServer(d),
	}
}

func (ids *Server) GetPluginCapabilities(
	_ context.Context,
	_ *csi.GetPluginCapabilitiesRequest,
) (*csi.GetPluginCapabilitiesResponse, error) {
	caps := []*csi.PluginCapability{
		{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
				},
			},
		},
		{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
				},
			},
		},
		{
			Type: &csi.PluginCapability_VolumeExpansion_{
				VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
					Type: csi.PluginCapability_VolumeExpansion_ONLINE,
				},
			},
		},
		{
			Type: &csi.PluginCapability_VolumeExpansion_{
				VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
					Type: csi.PluginCapability_VolumeExpansion_OFFLINE,
				},
			},
		},
	}

	// VolumeGroupSnapshot support (design §9): advertised only when the driver
	// serves the GroupController service, so the csi-snapshotter routes group
	// snapshots here. Gated so a driver built without it (e.g., the sanity
	// harness) is not offered the generic group-snapshot conformance tests,
	// which assume arbitrary volumes group-snapshot, unlike this driver's
	// persistent placement-pinned groups (§3, §11.6).
	if ids.Driver.GroupControllerEnabled() {
		caps = append(caps, &csi.PluginCapability{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE,
				},
			},
		})
	}

	return &csi.GetPluginCapabilitiesResponse{Capabilities: caps}, nil
}
