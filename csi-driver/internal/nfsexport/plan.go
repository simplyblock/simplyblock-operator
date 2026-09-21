// The volume stack an export's filesystem sits on.
//
// It is the same two layers the block path stages (the namespace attached, then
// XFS formatted and mounted), so a metadata-server host and a client node treat
// one namespace the same way. What is different is only who asks: no CSI call
// ever targets the MDS, because kubelet stages on the nodes running the pods, so
// assembly is the one thing that brings this stack up.

package nfsexport

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/klog"

	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
	"github.com/simplyblock/atlas/volstack/plans"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/initiator"
)

// layerFabric is the bottom layer's recorded name, read back on the teardown
// path. Stable across releases, because a teardown replays an older record.
const layerFabric = "fabric"

// planner builds an export's stack. The seams are the host's and resolved once,
// and only the identity a connect presents is per volume.
type planner struct {
	seams   plans.NodeConfig
	hostNQN HostNQNFunc
	store   *volstack.Store
}

// Plan is fabric → filesystem, mounted at the export path.
func (p planner) Plan(ctx context.Context, spec export.Spec) (volstack.Plan, error) {
	connection, hostNQN, err := p.connection(ctx, spec)
	if err != nil {
		return nil, err
	}

	cfg := p.seams
	cfg.HostNQN = hostNQN
	node := plans.NewNode(cfg)

	return node.Plain(connection, plans.Volume{
		UUID:        spec.VolumeUUID,
		StagingPath: spec.Path,
		FsType:      export.FSType,
		// Pinned rather than derived: this image's mkfs.xfs defaults features an
		// older host kernel cannot mount (format.go).
		FormatOptions: xfsFormatOptions,
		Encrypted:     spec.Encrypted,
	}), nil
}

// connection is where the namespace is published, and the identity to present.
//
// The control plane is asked first, because it is the only party that resolves
// a host's DHCHAP secret and the only one that knows where the volume moved
// after a failover.
func (p planner) connection(ctx context.Context, spec export.Spec) (lvol.Connection, string, error) {
	if spec.ClusterID == "" || spec.VolumeUUID == "" {
		return lvol.Connection{}, "", fmt.Errorf(
			"export %s: a stack cannot be planned without a cluster and a volume id", spec.Path)
	}

	var identity string
	if p.hostNQN != nil {
		identity = p.hostNQN(ctx)
	}

	if connection, named, ok := p.published(ctx, spec, identity); ok {
		if named == "" {
			named = identity
		}
		return connection, named, nil
	}

	// The stack record names the namespace this host attached, and carries no
	// address. That is enough to release one and not enough to attach one, which
	// is the right split: a teardown has to work when the control plane does
	// not, and the fabric layer finds its device by identity.
	connection, ok := p.recorded(spec)
	if !ok {
		return lvol.Connection{}, "", fmt.Errorf(
			"export %s: the control plane did not answer and no stack record names volume %s, "+
				"so there is nothing to assemble or to release", spec.Path, spec.VolumeUUID)
	}
	return connection, identity, nil
}

// published asks the control plane, reporting whether it answered. A refusal is
// not an error here: the caller has a weaker answer to fall back on.
func (p planner) published(
	ctx context.Context, spec export.Spec, identity string,
) (lvol.Connection, string, bool) {
	client, err := clusters.Client(ctx, spec.ClusterID, spec.PoolID)
	if err != nil {
		klog.Warningf("export %s: no control-plane client for cluster %s: %v",
			spec.Path, spec.ClusterID, err)
		return lvol.Connection{}, "", false
	}
	responses, err := client.LvolConnections(ctx, spec.VolumeUUID, identity)
	if err != nil || len(responses) == 0 {
		klog.Warningf("export %s: the control plane published no endpoint for volume %s: %v",
			spec.Path, spec.VolumeUUID, err)
		return lvol.Connection{}, "", false
	}
	connection, named := initiator.ConnectionFrom(responses, spec.VolumeUUID)
	return connection, named, true
}

// recorded is the namespace as the stack record names it.
func (p planner) recorded(spec export.Spec) (lvol.Connection, bool) {
	record, err := p.store.Load(spec.StackHandle())
	if err != nil {
		return lvol.Connection{}, false
	}
	for _, entry := range record.Plan {
		if entry.Layer != layerFabric || len(entry.Params) == 0 {
			continue
		}
		var params layers.FabricParams
		if err := json.Unmarshal(entry.Params, &params); err != nil {
			return lvol.Connection{}, false
		}
		return lvol.Connection{NQN: params.NQN, NSID: params.NSID, UUID: spec.VolumeUUID}, true
	}
	return lvol.Connection{}, false
}
