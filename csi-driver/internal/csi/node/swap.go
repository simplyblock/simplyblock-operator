// The node's swap service: re-points a staged volume's dm-linear mapping when
// the operator moves its namespace to another NVMe-oF subsystem
// (design-cg-live-subsystem-flip.md §6). It is built here, over the same seams
// and stack records the node stages with, because a swap has to agree with the
// stage about which mapping and which fabric a volume has.

package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/devmapper"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/volstack/swaprpc"

	"github.com/simplyblock/csi-driver/internal/controlplane"
)

// SwapperConfig is what the swap service needs beyond the node's own seams.
type SwapperConfig struct {
	// NodeName labels the metrics.
	NodeName string
	// Registerer receives the swap metrics. Nil registers none.
	Registerer prometheus.Registerer
}

// swapGate is what the node asks before it touches a volume's stack: whether
// the swap service is holding that volume's I/O. *swaprpc.Swapper is the real
// one.
type swapGate interface {
	Held(handle string) bool
}

// refuseWhileHeld is the answer to a stage, unstage, publish, or expand of a
// volume whose mapping the swap service is holding suspended. Each of those
// would act on the mapping under the swap: a grow or a heal reloads and
// resumes it, which lets I/O through to a namespace that is leaving its
// subsystem, and an unstage removes it or blocks on it. The window is bounded
// by the swap's deadline, so the RPC is refused as Unavailable and kubelet
// retries it once the window has closed. Called under the volume's lock, so a
// Suspend that lands after this check waits for the RPC to finish instead.
func (ns *Server) refuseWhileHeld(volumeID string) error {
	if ns.swapGate == nil || !ns.swapGate.Held(volumeID) {
		return nil
	}
	return status.Errorf(codes.Unavailable,
		"volume %s is holding its I/O for a subsystem swap on this node; retry after the window", volumeID)
}

// stackError is the gRPC status of a failure to plan or heal a volume's stack.
// A control plane holding the volume's subsystem for a move is transient and
// answered Unavailable, which kubelet retries; anything else is Internal.
func stackError(err error, format string, args ...any) error {
	code := codes.Internal
	if errors.Is(err, controlplane.ErrVolumeHeldByMove) {
		code = codes.Unavailable
	}
	return status.Errorf(code, format+": %v", append(args, err)...)
}

// NewSwapper builds the node's swap service over the staging records and the
// host seams this server stages with. The service takes the node's lock on a
// volume for its work on it, and from then on the node asks the service
// whether a volume is held before touching its stack.
func (ns *Server) NewSwapper(cfg SwapperConfig) (*swaprpc.Swapper, error) {
	seams := ns.stack.seams
	subsystems := nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})
	swap := swaprpc.Config{
		Records:        ns.stack.store,
		Mapper:         devmapper.New(nil),
		Devices:        seams.Devices,
		Connector:      seams.Connector,
		Rescan:         swaprpc.SysfsRescan(subsystems),
		DropStalePaths: swaprpc.SysfsDropStale(subsystems),
	}
	if ns.volumeLocks != nil {
		swap.Lock = ns.volumeLocks.Lock
	}
	if ns.kubeClient != nil {
		broadcaster := record.NewBroadcaster()
		broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: ns.kubeClient.CoreV1().Events("")})
		swap.Events = claimEvents{
			kube:     ns.kubeClient,
			recorder: broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "spdkcsi-node", Host: cfg.NodeName}),
		}
	}
	if cfg.Registerer != nil {
		metrics, err := swaprpc.NewPrometheusMetrics(cfg.Registerer, cfg.NodeName)
		if err != nil {
			return nil, fmt.Errorf("swap metrics: %w", err)
		}
		swap.Metrics = metrics
	}
	sw, err := swaprpc.New(swap)
	if err != nil {
		return nil, err
	}
	ns.swapGate = sw
	return sw, nil
}

// claimEvents reports a swap the node made on its own on the claim of the
// volume, which is the object the user who owns the pod watches. A volume with
// no claim has nobody to tell, and the node log carries it instead.
type claimEvents struct {
	kube     kubernetes.Interface
	recorder record.EventRecorder
}

// Warning emits a Warning on the claim of the volume staged under handle.
func (e claimEvents) Warning(handle, reason, message string) {
	klog.Warningf("volume %s: %s: %s", handle, reason, message)
	claim, err := e.claimOf(context.Background(), handle)
	if err != nil || claim == nil {
		return
	}
	e.recorder.Event(claim, corev1.EventTypeWarning, reason, message)
}

// claimOf is the claim of the PersistentVolume carrying handle. The lookup
// lists every volume, which is affordable because it runs only when the node
// had to act on its own.
func (e claimEvents) claimOf(ctx context.Context, handle string) (*corev1.ObjectReference, error) {
	pvs, err := e.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, pv := range pvs.Items {
		if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle != handle || pv.Spec.ClaimRef == nil {
			continue
		}
		ref := *pv.Spec.ClaimRef
		if ref.Kind == "" {
			ref.Kind = "PersistentVolumeClaim"
		}
		return &ref, nil
	}
	return nil, nil
}
