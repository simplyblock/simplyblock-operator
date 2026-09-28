// Attaching the backing namespace of a pNFS export to a client host.
//
// A client is an NVMe-oF initiator for the same namespace the metadata server
// made the filesystem on, which is the arrangement that lets the data path
// bypass it. This is the driver's ordinary connect path with a different
// caller, so reconnect handling, fabric repair, and the device wait come with
// it.

package nfsexport

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/nqn"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/initiator"
)

// HostNQNFunc reports this host's NQN, which the control plane checks against
// a volume's allowed_hosts. Empty is legitimate: a volume with none needs no
// identity.
type HostNQNFunc func(ctx context.Context) string

// HostNQN derives this host's NQN from the node UID, exactly as the block
// staging path does, so the control plane sees one identity however the
// namespace was connected. Two would each hold their own reservation key, and
// the fencing in §13.2 is written against one key per host.
//
// An empty answer is not a failure: a connect that needed one fails with the
// control plane's own message.
func HostNQN(nodeName string, kubeClient kubernetes.Interface) HostNQNFunc {
	return func(ctx context.Context) string {
		if kubeClient == nil || nodeName == "" {
			return ""
		}
		node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("pnfs: reading node %s for the host NQN: %v", nodeName, err)
			return ""
		}
		return nqn.Host(string(node.UID))
	}
}

// attacher connects and disconnects the namespace behind an export.
type attacher struct {
	hostNQN HostNQNFunc
}

// volumeContextFor is what the initiator is built from. The control plane's
// answer is merged last and wins: after a failover the local identifiers still
// name the volume while the target has moved.
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

// connection builds the initiator. One builder for both, because a disconnect
// addressed to a differently built initiator does not find the connection.
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

// Attach makes the namespace present. Idempotent: the reconciler driving it
// calls Create on every pass.
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

// Attach and Detach as standalone calls, for the node plugin's pNFS staging
// path: a client attaches the same namespace, because that is what lets it
// address the layout directly.
//
// The metadata server no longer comes through here, because its assembly
// attaches through the volume stack's fabric layer (plan.go), so this is one
// implementation short of the two paths sharing one. Folding the client onto a
// RawBlock plan is the follow-up, and it also collapses the separate sysfs
// lookup stagePNFSVolume does for the NGUID.
func Attach(ctx context.Context, spec export.Spec, hostNQN HostNQNFunc) error {
	return attacher{hostNQN: hostNQN}.Attach(ctx, spec)
}

func Detach(ctx context.Context, spec export.Spec, hostNQN HostNQNFunc) error {
	return attacher{hostNQN: hostNQN}.Detach(ctx, spec)
}
