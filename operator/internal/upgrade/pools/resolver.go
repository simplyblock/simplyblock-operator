// Resolving a pool's name to its UUID, which is what §16.4's handles need and
// what only the control plane knows.
//
// It is a package of its own rather than a function in the steps, because it is
// the one place in this tool that talks to something other than Kubernetes, and
// the credentials it uses are per cluster. An installation holds several, each
// with its own secret, and asking the wrong one is worse than asking nothing: a
// pool called production exists in two clusters and the answer would look
// normalized while naming a pool the volume is not in.

package pools

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/errs"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// Resolver answers a pool name from the control plane, using the credentials
// the cluster that holds the pool keeps beside its own object.
type Resolver struct {
	// Client reads the StorageCluster that reports the UUID a handle names, and
	// the Secret holding its token.
	Client client.Client

	// Endpoint is the control-plane management API. It is supplied rather than
	// discovered, because the source model records it nowhere a tool running
	// outside the cluster can read: the operator takes it from its own
	// environment, and a v1alpha1 ControlPlane carries no endpoint at all.
	Endpoint string

	// mu guards pools, which is one listing per cluster. A run asks about one
	// pool per volume and a cluster has thousands of volumes and a handful of
	// pools, so the listing is the same answer every time.
	mu    sync.Mutex
	pools map[string]map[string]string
}

// PoolUUID returns the UUID of the named pool in the named cluster.
//
// Both halves of a miss wrap errs.ErrNotFound, and both are findings rather
// than failures: a cluster no object reports is a handle naming an installation
// this is not, and a pool nobody answers to is a handle naming a pool that has
// been deleted. §16.4 reports either and does not proceed.
func (r *Resolver) PoolUUID(ctx context.Context, clusterUUID, poolName string) (string, error) {
	byName, err := r.listing(ctx, clusterUUID)
	if err != nil {
		return "", err
	}
	uuid, known := byName[poolName]
	if !known {
		return "", fmt.Errorf("no pool in cluster %s is called %q: %w",
			clusterUUID, poolName, errs.ErrNotFound)
	}
	return uuid, nil
}

// listing is one cluster's pools by name, read once.
func (r *Resolver) listing(ctx context.Context, clusterUUID string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if byName, cached := r.pools[clusterUUID]; cached {
		return byName, nil
	}

	token, err := r.token(ctx, clusterUUID)
	if err != nil {
		return nil, err
	}
	api, err := controlplane.New(controlplane.Config{Endpoint: r.Endpoint, Token: token})
	if err != nil {
		return nil, fmt.Errorf("reach the control plane at %s: %w", r.Endpoint, err)
	}
	found, err := api.ListStoragePools(ctx, clusterUUID)
	if err != nil {
		return nil, fmt.Errorf("list the pools of cluster %s: %w", clusterUUID, err)
	}

	byName := make(map[string]string, len(found))
	for _, pool := range found {
		byName[pool.Name] = pool.ID
	}
	if r.pools == nil {
		r.pools = map[string]map[string]string{}
	}
	r.pools[clusterUUID] = byName
	return byName, nil
}

// token is the control-plane secret of the cluster reporting this UUID.
//
// The cluster is found by what it reports rather than by what it is called,
// because a handle carries the backend's identifier and two namespaces may each
// hold a StorageCluster called prod.
func (r *Resolver) token(ctx context.Context, clusterUUID string) (string, error) {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := r.Client.List(ctx, &clusters); err != nil {
		return "", fmt.Errorf("list the StorageClusters: %w", err)
	}

	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if cluster.Status.UUID != clusterUUID {
			continue
		}
		var secret corev1.Secret
		key := client.ObjectKey{
			Namespace: cluster.Namespace,
			Name:      "simplyblock-cluster-" + cluster.Name,
		}
		if err := r.Client.Get(ctx, key, &secret); err != nil {
			return "", fmt.Errorf("read the credentials of cluster %s/%s: %w",
				cluster.Namespace, cluster.Name, err)
		}
		return string(secret.Data["secret"]), nil
	}

	return "", fmt.Errorf("no StorageCluster reports the cluster %s a handle names: %w",
		clusterUUID, errs.ErrNotFound)
}
