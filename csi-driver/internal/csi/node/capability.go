// Advertising this node's VDO capability (issue #277 §4.3): reading the
// postStart hook's probe result and turning it into the node label the
// topology gate (controller-side vdoCapableSegment) and an operator both key
// off.
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

// AdvertiseVDOCapability reads markerPath (kube.VDOCapableMarkerPath in
// production) and patches nodeName's storage.simplyblock.io/vdo-capable label
// to match, stamping it with kube.AnnoVDOCapableManagedBy so a later run of
// this same probe knows the label is its own to overwrite.
//
// An operator's hand-set label — present without that annotation — is left
// untouched: it is the escape hatch a golden-image node depends on, and this
// probe only manages labels it or an earlier version of it wrote.
//
// Runs once, at process start (see internal/driver.startNodeServer).
// Re-checking on an interval, so a node that gains capability without a
// restart is not stuck advertising false, is open (design-issue-277 §14 Q12).
func AdvertiseVDOCapability(ctx context.Context, kubeClient kubernetes.Interface, nodeName, markerPath string) error {
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read vdo-capable marker %s: %w", markerPath, err)
	}
	capable := strings.TrimSpace(string(marker)) == "true"

	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}

	_, hasLabel := node.Labels[kube.LabelVDOCapable]
	_, managedByThisProbe := node.Annotations[kube.AnnoVDOCapableManagedBy]
	if hasLabel && !managedByThisProbe {
		// An operator's own label. Not this probe's to manage.
		return nil
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
