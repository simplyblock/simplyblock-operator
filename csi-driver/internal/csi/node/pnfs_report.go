// Reporting a pNFS staging mount whose I/O keeps going through the metadata
// server, or whose layout probe does not return.
//
// Such a mount keeps working: every READ and WRITE goes through the metadata
// server instead of the device. Nothing fails, so without a report nobody sees
// it. This lives beside the primer because the primer is what notices.

package node

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
)

// The reasons of the events a report records: the mount's I/O has gone
// through the metadata server, or its layout probe has not returned.
const (
	reasonDataThroughMDS = "PNFSDataThroughMDS"
	reasonProbeHung      = "PNFSLayoutProbeHung"
)

// NewLayoutReporter returns the report KeepLayoutsPrimed takes: a Warning
// event on the claim of the volume, recorded as this node's plugin. Without a
// Kubernetes client it returns nil, and the primer only logs.
func NewLayoutReporter(kube kubernetes.Interface, nodeName string) reportFunc {
	if kube == nil {
		return nil
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme,
		corev1.EventSource{Component: "simplyblock-csi-node", Host: nodeName})
	return func(ctx context.Context, u unusableLayout) error {
		return reportUnusableLayout(ctx, kube, recorder, nodeName, u)
	}
}

// reportUnusableLayout records a Warning event on the claim of the volume
// mounted at u.Path, or on its PersistentVolume when it has no claim. The
// volume ID is the last element of the staging mount's path, as in
// reconcileDeviceAliases.
func reportUnusableLayout(
	ctx context.Context, kube kubernetes.Interface, recorder record.EventRecorder, nodeName string, u unusableLayout,
) error {
	volumeID := filepath.Base(u.Path)
	pvs, err := kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing PersistentVolumes: %w", err)
	}
	var pv *corev1.PersistentVolume
	for i := range pvs.Items {
		if csi := pvs.Items[i].Spec.CSI; csi != nil && csi.VolumeHandle == volumeID {
			pv = &pvs.Items[i]
			break
		}
	}
	if pv == nil {
		return fmt.Errorf("no PersistentVolume has the volume handle %s", volumeID)
	}

	var object runtime.Object = pv
	if ref := pv.Spec.ClaimRef; ref != nil {
		object = &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: ref.Name, Namespace: ref.Namespace, UID: ref.UID,
		}}
	}
	if u.Hung {
		recorder.Eventf(object, corev1.EventTypeWarning, reasonProbeHung,
			"The pNFS layout probe of this volume on node %s has not returned for %s. It is likely blocked in "+
				"the kernel, and the node does not probe the volume again until it returns.",
			nodeName, u.For.Round(time.Second))
		return nil
	}
	// The cause is not named: a client that cannot open the device, a fenced
	// one, and a pod doing I/O the layout does not serve look alike from the
	// node (see the primer's file comment).
	recorder.Eventf(object, corev1.EventTypeWarning, reasonDataThroughMDS,
		"The pNFS client on node %s has sent this volume's I/O through the metadata server for %s instead "+
			"of the block device. Either the client cannot use the layout's device, or the workload issues "+
			"I/O the layout does not serve, such as unaligned direct I/O. Last layout probe: %s.",
		nodeName, u.For.Round(time.Second), probeOutcome(u))
	return nil
}

// probeOutcome is how the mount's last finished probe ended, for a log line
// or an event.
func probeOutcome(u unusableLayout) string {
	switch {
	case !u.Probed:
		return "none has finished"
	case u.ProbeErr == nil:
		return "took the block layout"
	default:
		return u.ProbeErr.Error()
	}
}
