// Reads the postStart hook's VDO probe result and turns it into a node label
// (issue #277 §4.3).
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/simplyblock/atlas/kube"
)

// vdoCapableTrue is the marker file's positive content.
const vdoCapableTrue = "true"

// AdvertiseVDOCapability reads markerPath (kube.VDOCapableMarkerPath in
// production) and sets nodeName's vdo-capable label to match.
//
// An operator's hand-set label (no managed-by annotation) is left alone —
// that's the override an admin uses to skip auto-detection.
//
// Runs once at process start. No periodic re-check yet (§14 Q12).
func AdvertiseVDOCapability(ctx context.Context, kubeClient kubernetes.Interface, nodeName, markerPath string) error {
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read vdo-capable marker %s: %w", markerPath, err)
	}
	capable := strings.TrimSpace(string(marker)) == vdoCapableTrue

	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}

	_, hasLabel := node.Labels[kube.LabelVDOCapable]
	_, managedByThisProbe := node.Annotations[kube.AnnoVDOCapableManagedBy]
	if hasLabel && !managedByThisProbe {
		return nil // an operator's own label
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"labels":      map[string]string{kube.LabelVDOCapable: strconv.FormatBool(capable)},
			"annotations": map[string]string{kube.AnnoVDOCapableManagedBy: kube.AnnoVDOCapableManagedByAutoDetect},
		},
	})
	if err != nil {
		return fmt.Errorf("build vdo-capable label patch: %w", err)
	}

	if _, err := kubeClient.CoreV1().Nodes().Patch(
		ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patch node %s with vdo-capable=%t: %w", nodeName, capable, err)
	}
	return nil
}
