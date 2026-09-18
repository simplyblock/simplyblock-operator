// Attaching the backing namespace of a pNFS export to this host.
//
// The metadata server is an NVMe-oF initiator for the volume exactly like a
// client is -- that is the whole arrangement, and it is why the data path can
// bypass the metadata server at all. What is different about the MDS host is
// that no CSI call ever targets it: kubelet stages the volume on the nodes
// running the pods, never on the host serving the export. So unless assembly
// attaches the namespace, nothing on that host will, and the filesystem has no
// device to live on.
//
// This is the driver's ordinary connect path, reached from a different caller.
// It asks the control plane where the namespace is served from and hands the
// answer to the same initiator NodeStageVolume uses, so reconnect handling,
// fabric repair, and the device wait all come with it rather than being
// reimplemented behind the export service.

package nfsexport

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/export"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/initiator"
)

// HostNQNFunc reports this host's NVMe qualified name, which the control plane
// checks against a volume's allowed_hosts before it will say where the
// namespace is served from. An empty string is legitimate: a volume with no
// allowed_hosts does not need one.
type HostNQNFunc func(ctx context.Context) string

// attacher connects and disconnects the namespace behind an export.
type attacher struct {
	hostNQN HostNQNFunc
}

// volumeContextFor is what the initiator is constructed from.
//
// The control plane's answer is merged last and wins, because it is the
// authority on where the namespace is served from: after a failover the local
// identifiers still name the volume correctly while the target has moved, and a
// local value that overrode the answer would connect to the old one.
func volumeContextFor(spec export.Spec, hostNQN string, info map[string]string) map[string]string {
	vc := map[string]string{
		"uuid":       spec.VolumeUUID,
		"cluster_id": spec.ClusterID,
		"poolID":     spec.PoolID,
	}
	if hostNQN != "" {
		vc["hostNQN"] = hostNQN
	}
	for k, v := range info {
		vc[k] = v
	}
	return vc
}

// connection builds the initiator for a spec, which both attach and detach need
// and neither should assemble differently: a disconnect addressed to a
// differently built initiator is a disconnect that does not find the connection.
func (a attacher) connection(ctx context.Context, spec export.Spec) (initiator.Initiator, error) {
	if spec.ClusterID == "" || spec.VolumeUUID == "" {
		return nil, fmt.Errorf(
			"export %s: the namespace cannot be attached without a cluster and a volume id", spec.Path)
	}
	client, err := clusters.Client(ctx, spec.ClusterID, spec.PoolID)
	if err != nil {
		return nil, fmt.Errorf("export %s: reaching cluster %s: %w", spec.Path, spec.ClusterID, err)
	}

	var hostNQN string
	if a.hostNQN != nil {
		hostNQN = a.hostNQN(ctx)
	}
	info, err := client.VolumeInfo(ctx, spec.VolumeUUID, hostNQN)
	if err != nil {
		return nil, fmt.Errorf("export %s: connection info for volume %s: %w",
			spec.Path, spec.VolumeUUID, err)
	}
	return initiator.New(volumeContextFor(spec, hostNQN, info))
}

// Attach makes the namespace present. It is idempotent, because the reconciler
// driving it calls Create on every pass.
func (a attacher) Attach(ctx context.Context, spec export.Spec) error {
	conn, err := a.connection(ctx, spec)
	if err != nil {
		return err
	}
	if _, err := conn.Connect(ctx); err != nil {
		return fmt.Errorf("export %s: connecting volume %s: %w", spec.Path, spec.VolumeUUID, err)
	}
	return nil
}

// Detach gives the namespace up, after the export has been unmounted.
func (a attacher) Detach(ctx context.Context, spec export.Spec) error {
	conn, err := a.connection(ctx, spec)
	if err != nil {
		return err
	}
	if err := conn.Disconnect(ctx); err != nil {
		return fmt.Errorf("export %s: disconnecting volume %s: %w", spec.Path, spec.VolumeUUID, err)
	}
	return nil
}

// Attach and Detach are the same operations as a standalone call, for the node
// plugin's staging path.
//
// A client connects the namespace for the same reason the metadata server does,
// and one implementation serves both so that the two cannot disagree about how
// a namespace is connected or under which host identity. Two identities for one
// host would each hold their own reservation key, and the fencing in
// design-pnfs-rwx.md §13.2 is written against one key per host.
func Attach(ctx context.Context, spec export.Spec, hostNQN HostNQNFunc) error {
	return attacher{hostNQN: hostNQN}.Attach(ctx, spec)
}

func Detach(ctx context.Context, spec export.Spec, hostNQN HostNQNFunc) error {
	return attacher{hostNQN: hostNQN}.Detach(ctx, spec)
}
