// Reporting a pNFS staging mount whose block layout this node cannot use.
//
// Such a mount keeps working: every READ and WRITE goes through the metadata
// server instead of the device. Nothing fails, so without a report nobody sees
// it. This lives beside the primer because the primer is what notices: its
// probes of the mount keep going through the server too.

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
	"k8s.io/klog"
)

// reasonDataThroughMDS is the reason of the event a report records.
const reasonDataThroughMDS = "PNFSDataThroughMDS"

// NewLayoutReporter returns the report KeepLayoutsPrimed takes: a Warning
// event on the claim of the volume, recorded as this node's plugin. Without a
// Kubernetes client it returns nil, and the primer only logs.
func NewLayoutReporter(
	kube kubernetes.Interface, nodeName string,
) func(ctx context.Context, path string, failingFor time.Duration, err error) {
	if kube == nil {
		return nil
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme,
		corev1.EventSource{Component: "simplyblock-csi-node", Host: nodeName})
	return func(ctx context.Context, path string, failingFor time.Duration, err error) {
		u := unusableLayout{Path: path, FailingFor: failingFor, Err: err}
		if rerr := reportUnusableLayout(ctx, kube, recorder, nodeName, u); rerr != nil {
			klog.Warningf("pnfs: could not report %s: %v", path, rerr)
		}
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
	recorder.Eventf(object, corev1.EventTypeWarning, reasonDataThroughMDS,
		"The pNFS client on node %s has sent this volume's I/O through the metadata server for %s, "+
			"because its probes cannot take the block layout: %v",
		nodeName, u.FailingFor.Round(time.Second), u.Err)
	return nil
}
