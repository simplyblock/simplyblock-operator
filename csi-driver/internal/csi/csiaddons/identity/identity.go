// Package identity serves the csi-addons Identity service: GetIdentity,
// GetCapabilities (advertising VOLUME_REPLICATION), and Probe. It is a
// distinct service from the CSI spec's own Identity (served by
// internal/csi/identity), not an extension of it, because the
// kubernetes-csi-addons controller-manager and the CSI sidecars probe two
// separate protocols on the same socket.
package identity

import (
	"context"

	"github.com/csi-addons/spec/lib/go/identity"
)

// Server implements the csi-addons Identity service.
type Server struct {
	identity.UnimplementedIdentityServer
	name    string
	version string
}

// New returns an identity.Server reporting name and version as this driver's
// own (the same values the CSI spec's Identity service reports), since both
// protocols identify the one plugin process serving them.
func New(name, version string) *Server {
	return &Server{name: name, version: version}
}

func (s *Server) GetIdentity(context.Context, *identity.GetIdentityRequest) (*identity.GetIdentityResponse, error) {
	return &identity.GetIdentityResponse{Name: s.name, VendorVersion: s.version}, nil
}

func (s *Server) GetCapabilities(
	context.Context, *identity.GetCapabilitiesRequest,
) (*identity.GetCapabilitiesResponse, error) {
	return &identity.GetCapabilitiesResponse{
		Capabilities: []*identity.Capability{
			{
				Type: &identity.Capability_Service_{
					Service: &identity.Capability_Service{
						Type: identity.Capability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &identity.Capability_VolumeReplication_{
					VolumeReplication: &identity.Capability_VolumeReplication{
						Type: identity.Capability_VolumeReplication_VOLUME_REPLICATION,
					},
				},
			},
			// The VolumeGroup service (design §14.3), which the stock
			// kubernetes-csi-addons controller-manager dials to form a backend
			// consistency group before replicating it as one unit.
			{
				Type: &identity.Capability_VolumeGroup_{
					VolumeGroup: &identity.Capability_VolumeGroup{
						Type: identity.Capability_VolumeGroup_VOLUME_GROUP,
					},
				},
			},
			{
				Type: &identity.Capability_VolumeGroup_{
					VolumeGroup: &identity.Capability_VolumeGroup{
						Type: identity.Capability_VolumeGroup_MODIFY_VOLUME_GROUP,
					},
				},
			},
			// DeleteVolumeGroup dissolves the group but keeps its member volumes
			// (design §14.3): a group is a label-formed set, never an owner of
			// the volumes' lifecycle.
			{
				Type: &identity.Capability_VolumeGroup_{
					VolumeGroup: &identity.Capability_VolumeGroup{
						Type: identity.Capability_VolumeGroup_DO_NOT_ALLOW_VG_TO_DELETE_VOLUMES,
					},
				},
			},
		},
	}, nil
}

func (s *Server) Probe(context.Context, *identity.ProbeRequest) (*identity.ProbeResponse, error) {
	// Ready left nil: per the spec, absent means "assume ready." The
	// Replication service's RPCs are stateless and idempotent by contract
	// (design §5), so there is no initialization phase to report against.
	return &identity.ProbeResponse{}, nil
}
