// Package cpadapter connects the two-node arbitration controller to the
// control plane's arbitration API through the operator's existing client: the
// endpoint the ControlPlane resolves (local, or a managed hub) and the cluster's
// own recorded secret, the same way the pool and cluster controllers reach a
// managed control plane.
package cpadapter

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/controlplane"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/twonode"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// Adapter implements twonode.API against the control plane.
type Adapter struct {
	Reader           client.Reader
	EndpointResolver controlplane.EndpointResolver

	mu       sync.Mutex
	startup  *webapi.Client
	resolved *webapi.Client
}

var _ twonode.API = (*Adapter)(nil)

// Get reads the arbiter's record for the cluster.
func (a *Adapter) Get(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster) (*twonode.Record, error) {
	ctx = a.authenticate(ctx, cluster)
	rec, err := a.client(ctx).GetClusterArbitration(ctx, cluster.Status.UUID)
	if err != nil || rec == nil {
		return nil, err
	}
	out := &twonode.Record{State: rec.State, Epoch: rec.Epoch, FencedNodes: rec.FencedNodes(),
		Enabled: rec.Enabled, HostFencing: rec.HostFencing, ClientHosts: rec.ClientHosts}
	if len(rec.HostFences) > 0 {
		out.HostFences = make(map[string]twonode.HostFence, len(rec.HostFences))
		for id, f := range rec.HostFences {
			out.HostFences[id] = twonode.HostFence{State: f.State, On: f.On, Epoch: f.Epoch,
				Since: time.UnixMilli(f.Since)}
		}
	}
	return out, nil
}

// SetTwoNodeSettings writes the cluster's two-node settings.
func (a *Adapter) SetTwoNodeSettings(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, arbitration, hostFencing bool,
) error {
	ctx = a.authenticate(ctx, cluster)
	return a.client(ctx).SetTwoNodeSettings(ctx, cluster.Status.UUID, arbitration, hostFencing)
}

// SetClientHosts reports the host NQNs of a storage node's Kubernetes node.
func (a *Adapter) SetClientHosts(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, nodeUUID string, hostNQNs []string,
) error {
	ctx = a.authenticate(ctx, cluster)
	return a.client(ctx).SetArbitrationClientHosts(ctx, cluster.Status.UUID, nodeUUID, hostNQNs)
}

// ReleaseHostFence gives a fenced node's host access back.
func (a *Adapter) ReleaseHostFence(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, nodeUUID string,
) error {
	ctx = a.authenticate(ctx, cluster)
	return a.client(ctx).ReleaseHostFence(ctx, cluster.Status.UUID, nodeUUID)
}

// ReportFencing sends positive fencing evidence for a node, or clears it.
func (a *Adapter) ReportFencing(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster, nodeUUID string, fenced bool,
) error {
	ctx = a.authenticate(ctx, cluster)
	return a.client(ctx).ReportArbitrationRemediation(ctx, cluster.Status.UUID, nodeUUID, fenced)
}

// authenticate scopes the call to the cluster's own secret, the only
// credential that reaches a control plane on another Kubernetes cluster.
func (a *Adapter) authenticate(ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster) context.Context {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: fmt.Sprintf("simplyblock-cluster-%s", cluster.Name)}
	if err := a.Reader.Get(ctx, key, &secret); err == nil {
		if s := string(secret.Data["secret"]); s != "" {
			return webapi.WithBearerToken(ctx, s)
		}
	}
	return ctx
}

func (a *Adapter) client(ctx context.Context) *webapi.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.startup == nil {
		a.startup = webapi.NewClient()
	}
	if a.EndpointResolver == nil {
		return a.startup
	}
	endpoint := a.EndpointResolver(ctx)
	if endpoint == "" || endpoint == a.startup.BaseURL {
		return a.startup
	}
	if a.resolved == nil || a.resolved.BaseURL != endpoint {
		a.resolved = webapi.NewClient(endpoint)
	}
	return a.resolved
}
