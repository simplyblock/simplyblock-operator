// The control-plane client the metadata server's state volume is managed
// through, per storage cluster. A cluster a SimplyblockDriver declares in
// spec.remoteClusters is reached at its own endpoint with its own secret; any
// other cluster goes through the operator's local control plane.
package driver

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	atlascp "github.com/simplyblock/atlas/controlplane"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/mdsstate"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// StateVolumeAPI returns the mdsstate.Coordinator's APIFor.
func StateVolumeAPI(c client.Client, local mdsstate.VolumeAPI) func(context.Context, string) (mdsstate.VolumeAPI, error) {
	return func(ctx context.Context, clusterID string) (mdsstate.VolumeAPI, error) {
		var drivers simplyblockv1alpha2.SimplyblockDriverList
		if err := c.List(ctx, &drivers); err != nil {
			return nil, fmt.Errorf("listing SimplyblockDrivers: %w", err)
		}
		for _, d := range drivers.Items {
			for _, item := range d.Spec.RemoteClusters {
				if item.ClusterID != clusterID {
					continue
				}
				r := &SimplyblockDriverReconciler{Client: c}
				secret, found, err := r.clusterSecret(ctx, d.Namespace, item.CredentialsSecretRef.Name)
				if err != nil {
					return nil, err
				}
				if !found {
					return nil, fmt.Errorf("remote cluster %s: Secret %s has no %s key",
						clusterID, item.CredentialsSecretRef.Name, remoteClusterSecretKey)
				}
				return atlascp.New(webapi.NewClient(item.Endpoint).AtlasConfig(webapi.WithBearerToken(ctx, secret)))
			}
		}
		return local, nil
	}
}
