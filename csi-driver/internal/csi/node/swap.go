// The node's swap service: re-points a staged volume's dm-linear mapping when
// the operator moves its namespace to another NVMe-oF subsystem
// (design-cg-live-subsystem-flip.md §6). It is built here, over the same seams
// and stack records the node stages with, because a swap has to agree with the
// stage about which mapping and which fabric a volume has.

package node

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
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
)

// SwapperConfig is what the swap service needs beyond the node's own seams.
type SwapperConfig struct {
	// NodeName labels the metrics.
	NodeName string
	// Registerer receives the swap metrics. Nil registers none.
	Registerer prometheus.Registerer
}

// NewSwapper builds the node's swap service over the staging records and the
// host seams this server stages with.
func (ns *Server) NewSwapper(cfg SwapperConfig) (*swaprpc.Swapper, error) {
	seams := ns.stack.seams
	swap := swaprpc.Config{
		Records:   ns.stack.store,
		Mapper:    devmapper.New(nil),
		Devices:   seams.Devices,
		Connector: seams.Connector,
		Rescan:    swaprpc.SysfsRescan(nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{})),
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
	return swaprpc.New(swap)
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
