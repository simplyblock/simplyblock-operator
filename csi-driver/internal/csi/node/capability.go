// Reads the postStart hook's VDO probe result and turns it into a node label
// (issue #277 §4.3).
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/kube"
)

// The wait for the postStart hook's answer, and how often it is looked for.
//
// The hook and the container's own entrypoint start at the same time, so the
// marker is normally absent for the first moments of this process's life:
// reading it once found nothing on every node of a cluster, and left every one
// of them unlabeled, which turns the feature off everywhere without saying so.
//
// The budget is generous because what is being waited for is a `modprobe`
// against a kernel that may be loading other things, and it is bounded because
// a hook that never writes the marker leaves a question nothing will answer.
const (
	markerWait = 2 * time.Minute
	markerPoll = time.Second
)

// awaitMarker reads the marker, waiting for the hook to write it.
//
// A marker that is not there yet is the expected state rather than a failure,
// and is the only one worth waiting through. Any other read error is answered
// immediately: a marker that exists and cannot be read is a permission or a
// mount problem, and waiting out the budget on it would report it as a timeout
// that says nothing about the cause.
func awaitMarker(ctx context.Context, path string, wait, poll time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		marker, err := os.ReadFile(path)
		switch {
		case err == nil:
			return string(marker), nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("read vdo-capable marker %s: %w", path, err)
		case !time.Now().Before(deadline):
			return "", fmt.Errorf(
				"the vdo-capable marker %s was not written within %s; the node plugin's postStart "+
					"probe either has not run or could not reach the host path it writes to", path, wait)
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for the vdo-capable marker %s: %w", path, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// vdoCapableTrue and vdoCapableFalse are the two things the marker file says,
// and the two values the label carries.
const vdoCapableFalse = "false"

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
	return advertiseVDOCapability(ctx, kubeClient, nodeName, markerPath, markerWait, markerPoll)
}

// advertiseVDOCapability is AdvertiseVDOCapability with the wait spelled out,
// so a test can drive it in milliseconds rather than minutes.
func advertiseVDOCapability(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	nodeName, markerPath string,
	wait, poll time.Duration,
) error {
	marker, err := awaitMarker(ctx, markerPath, wait, poll)
	if err != nil {
		return err
	}
	capable := strings.TrimSpace(marker) == vdoCapableTrue

	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}

	existing, hasLabel := node.Labels[kube.LabelVDOCapable]
	_, managedByThisProbe := node.Annotations[kube.AnnoVDOCapableManagedBy]
	if hasLabel && !managedByThisProbe {
		klog.Infof("node %s carries a hand-set %s=%s, which this probe leaves alone; "+
			"its own reading of %s was %q", nodeName, kube.LabelVDOCapable, existing, markerPath, marker)
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

	// Said out loud, at the level a passing run keeps, because this is the one
	// fact that decides whether a volume asking for client-side compression can
	// be placed at all. Silence on success is what made a cluster where no node
	// was capable indistinguishable from one where the probe never ran: the
	// volumes were simply unschedulable, with nothing anywhere having failed.
	klog.Infof("node %s labeled %s=%t, from the postStart probe's marker %s",
		nodeName, kube.LabelVDOCapable, capable, markerPath)
	return nil
}
