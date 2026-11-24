package reconnect

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
)

// volumeClient is the part of a control-plane client the reconnect loop reads
// through. *controlplane.ClusterClient satisfies it; tests substitute fakes.
type volumeClient interface {
	VolumeNodeInfo(ctx context.Context, lvolID string) (*controlplane.NodeInfo, error)
	LvolConnections(ctx context.Context, lvolID, hostNQN string) ([]*controlplane.LvolConnectResp, error)
	StorageNodeStatus(ctx context.Context, nodeID string) (string, error)
}

// clusterResolver finds the cluster a volume lives in now.
//
// The cluster an attached subsystem names (its NQN, and the PV handle it came
// from) is where the volume was created, and both are immutable. A
// cluster-scope online migration with --delete-source keeps the volume's UUID
// but moves it into a pool of another cluster, after which every lookup in the
// named cluster answers "not found in any pool". Without following it the loop
// never re-adds a lost path of a migrated volume, and once the last path goes
// the kernel removes the device for good (bf26, 2026-11-17: 1621 failed
// recoveries in 6 h).
//
// So a lookup that the named cluster answers with "volume not found" is tried
// against every other cluster in the driver's secret, by the same UUID, and the
// cluster that has it is remembered per lvol. Steady state is one lookup
// against the remembered cluster; a later not-found there drops the memory and
// searches again. Any other failure (transport, 5xx, auth) is returned as is:
// a control plane that cannot answer says nothing about where the volume is.
type clusterResolver struct {
	mu       sync.Mutex
	resolved map[string]string // lvolID -> cluster it was last found in, when not its named cluster

	newClient func(ctx context.Context, clusterID string) (volumeClient, error)
	list      func() ([]string, error)
}

func newClusterResolver() *clusterResolver {
	return &clusterResolver{
		resolved: map[string]string{},
		newClient: func(ctx context.Context, clusterID string) (volumeClient, error) {
			return clusters.Client(ctx, clusterID, "")
		},
		list: clusters.List,
	}
}

// volumeClusters is the process-wide resolver the reconnect loop uses.
var volumeClusters = newClusterResolver()

// isVolumeNotFound reports whether err says the volume is absent from the
// cluster asked, as opposed to the cluster failing to answer.
func isVolumeNotFound(err error) bool {
	var httpErr *controlplane.HTTPError
	return errors.Is(err, controlplane.ErrVolumeNotFound) ||
		(errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound)
}

func (r *clusterResolver) cached(lvolID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.resolved[lvolID]
	return c, ok
}

func (r *clusterResolver) remember(lvolID, namedCluster, foundCluster string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if foundCluster == namedCluster {
		delete(r.resolved, lvolID)
		return
	}
	r.resolved[lvolID] = foundCluster
}

func (r *clusterResolver) forget(lvolID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.resolved, lvolID)
}

// lookup runs query against the cluster lvolID lives in and returns the
// client that answered it, along with that cluster's ID. namedCluster is the
// cluster the subsystem's NQN (or the PV handle) names.
//
// The first attempt goes to the remembered cluster, or namedCluster when none
// is remembered. Only a not-found answer there (or a named cluster no longer
// in the secret) widens the search to the other clusters; when no cluster has
// the volume, the first attempt's error is returned unchanged.
func (r *clusterResolver) lookup(
	ctx context.Context, namedCluster, lvolID string, query func(volumeClient) error,
) (volumeClient, string, error) {
	first, remembered := r.cached(lvolID)
	if !remembered {
		first = namedCluster
	}

	client, err := r.newClient(ctx, first)
	if err == nil {
		err = query(client)
		if err == nil {
			return client, first, nil
		}
		if !isVolumeNotFound(err) {
			return nil, "", err
		}
	} else if !remembered && !errors.Is(err, controlplane.ErrClusterNotFound) {
		return nil, "", err
	}
	firstErr := err
	if remembered {
		r.forget(lvolID)
	}

	ids, lerr := r.list()
	if lerr != nil {
		klog.Warningf("reconnect: volume %s not in cluster %s, and the cluster list could not be read: %v",
			lvolID, first, lerr)
		return nil, "", firstErr
	}
	for _, id := range ids {
		if id == first {
			continue
		}
		c, cerr := r.newClient(ctx, id)
		if cerr != nil {
			klog.Warningf("reconnect: no client for cluster %s while locating volume %s: %v", id, lvolID, cerr)
			continue
		}
		qerr := query(c)
		if qerr == nil {
			if id != namedCluster {
				klog.Infof("reconnect: volume %s (named cluster %s) is served by cluster %s", lvolID, namedCluster, id)
			}
			r.remember(lvolID, namedCluster, id)
			return c, id, nil
		}
		if !isVolumeNotFound(qerr) {
			klog.Warningf("reconnect: cluster %s could not answer for volume %s: %v", id, lvolID, qerr)
		}
	}
	return nil, "", firstErr
}
