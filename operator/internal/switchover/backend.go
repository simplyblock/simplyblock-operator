// Package switchover is the operator's half of a stretched cluster's switchover
// (design-stretched-sync-replication-ramen.md §9.3): the backend the atlas-lib
// switchover server issues its sync verbs through. The server itself, and the
// sequencing of a move, is atlas-lib's switchover package; this is how the
// operator reaches the control plane for it.
package switchover

import (
	"context"
	"fmt"
	"net/http"

	atlaskube "github.com/simplyblock/atlas/kube"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/switchover"
)

// Backend reaches the control plane the ControlPlane object names, with the
// credential it names, resolved on every call: a changed endpoint or a rotated
// token reaches the next move without a restart.
type Backend struct {
	// Endpoint resolves the control plane's base URL.
	Endpoint func(context.Context) string
	// Credential resolves a managed control plane's credential, false when the
	// control plane is this cluster's own.
	Credential func(context.Context) (string, bool)
	// Transport carries the operator's TLS to the control plane. Nil is the
	// default transport.
	Transport http.RoundTripper
}

var _ switchover.Backend = (*Backend)(nil)

// client builds the atlas client for one call. A local control plane trusts
// this operator's ServiceAccount token; a managed one, on another cluster,
// cannot verify it and takes the credential the ControlPlane names.
func (b *Backend) client(ctx context.Context) (*controlplane.Client, error) {
	endpoint := b.Endpoint(ctx)
	if endpoint == "" {
		return nil, fmt.Errorf("switchover: the control plane's endpoint is not known yet")
	}
	token, managed := b.Credential(ctx)
	if !managed {
		var err error
		if token, err = atlaskube.ServiceAccountToken(); err != nil {
			return nil, fmt.Errorf("switchover: the service-account token: %w", err)
		}
	}
	return controlplane.New(controlplane.Config{Endpoint: endpoint, Token: token, Transport: b.Transport})
}

// Volume reads the volume, for the store its move is serialized on.
func (b *Backend) Volume(ctx context.Context, h lvol.VolumeHandle) (lvol.Volume, error) {
	c, err := b.client(ctx)
	if err != nil {
		return lvol.Volume{}, err
	}
	return c.Volume(ctx, h)
}

// SyncStatus reads where the volume is led from.
func (b *Backend) SyncStatus(ctx context.Context, h lvol.VolumeHandle) (controlplane.VolumeSyncStatus, error) {
	c, err := b.client(ctx)
	if err != nil {
		return controlplane.VolumeSyncStatus{}, err
	}
	return c.SyncStatus(ctx, h)
}

// SyncDemoteVolume demotes the volume on the site it leaves.
func (b *Backend) SyncDemoteVolume(ctx context.Context, h lvol.VolumeHandle, site string) error {
	c, err := b.client(ctx)
	if err != nil {
		return err
	}
	return c.SyncDemoteVolume(ctx, h, site)
}

// SyncPromoteVolume promotes the volume on the site it moves to.
func (b *Backend) SyncPromoteVolume(
	ctx context.Context, h lvol.VolumeHandle, site string, planned bool,
) (controlplane.SyncPromoteResult, error) {
	c, err := b.client(ctx)
	if err != nil {
		return controlplane.SyncPromoteResult{}, err
	}
	return c.SyncPromoteVolume(ctx, h, site, planned)
}
