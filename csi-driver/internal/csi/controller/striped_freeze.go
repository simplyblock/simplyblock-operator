// The controller's half of a striped volume's crash-consistent snapshot: having
// the node that stages the volume suspend it while every member is snapshotted
// or cloned (csi-driver/docs/striped-volumes.md).
//
// The protocol, from here:
//
//  1. Find where the volume is staged, from its VolumeAttachments. Nowhere means
//     nothing writes to it, and the members are taken without a freeze. More
//     than one node is refused: a striped volume is single-node.
//  2. Freeze(node, volume, token, deadline) through the operator's relay. The
//     node suspends the stripe's device-mapper device (I/O drains, new I/O
//     waits, a mounted filesystem is frozen and flushed) and resumes it by
//     itself at the deadline, whatever happens here.
//  3. Take every member concurrently, under a context that ends before the
//     deadline does.
//  4. Thaw(node, volume, token). The node resumes the device and answers whether
//     it stayed suspended from Freeze until then.
//  5. Only a freeze that held, and members that were all taken, is a success.
//     Anything else is returned as an error the caller retries, after the caller
//     removes what it took.
//
// The token is a fresh UUID per attempt, so a late Thaw from an attempt that
// gave up cannot thaw a later one, and a retried Freeze is the same freeze.

package controller

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

const (
	// defaultFreezeDeadline is how long a striped volume may stay suspended for
	// a snapshot before its node resumes it by itself. The members are taken
	// concurrently, so the freeze lasts about as long as the slowest member
	// snapshot, normally well under a second.
	defaultFreezeDeadline = 10 * time.Second

	// freezeDeadlineEnv overrides it, as a Go duration ("5s").
	freezeDeadlineEnv = "SPDKCSI_STRIPE_FREEZE_DEADLINE"

	// thawTimeout bounds the Thaw, which runs on a context of its own: the
	// snapshot's context may be the reason the attempt ended.
	thawTimeout = 30 * time.Second
)

// stripeFreezer freezes and thaws a volume on a node. csilink.Freezer is the
// implementation, over the operator's relay.
type stripeFreezer interface {
	Freeze(ctx context.Context, node, handle, token string, deadline time.Duration) (bool, error)
	Thaw(ctx context.Context, node, handle, token string) (bool, error)
}

// SetFreezer gives the server the way to freeze a striped volume on its node.
// Without one, a striped volume that is staged somewhere cannot be snapshotted
// or cloned, and the request is refused as Unavailable.
func (cs *Server) SetFreezer(f stripeFreezer) { cs.freezer = f }

// freezeDeadline is the deadline a freeze asks for.
func freezeDeadline() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(freezeDeadlineEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
		klog.Warningf("%s=%q is not a duration; using %v", freezeDeadlineEnv, raw, defaultFreezeDeadline)
	}
	return defaultFreezeDeadline
}

// takeWindow is how much of a freeze's deadline the members may take. The
// rest is left for the Thaw to reach the node before the node resumes the
// device by itself.
func takeWindow(deadline time.Duration) time.Duration {
	return deadline - deadline/5
}

// underFreeze runs take with the striped volume's writer stopped, and fails
// unless the freeze held for the whole of it.
func (cs *Server) underFreeze(ctx context.Context, volumeID string, take func(context.Context) error) error {
	nodes, err := cs.stagedNodes(ctx, volumeID)
	if err != nil {
		return err
	}
	switch len(nodes) {
	case 0:
		// Attached nowhere: nothing writes to the members.
		return take(ctx)
	case 1:
	default:
		return status.Errorf(codes.FailedPrecondition,
			"striped volume %s is attached to %d nodes (%v), and a consistent snapshot needs its one writer",
			volumeID, len(nodes), nodes)
	}
	node := nodes[0]
	if cs.freezer == nil {
		return status.Errorf(codes.Unavailable,
			"striped volume %s is attached to node %s, and without the link to the operator it cannot be "+
				"frozen there for a consistent snapshot", volumeID, node)
	}

	deadline := freezeDeadline()
	token := uuid.NewString()
	start := time.Now()
	frozen, err := cs.freezer.Freeze(ctx, node, volumeID, token, deadline)
	if err != nil {
		// The node may have frozen it and the answer been lost: thawing under
		// this token undoes that, and touches nothing else.
		cs.thaw(ctx, node, volumeID, token)
		if s, ok := status.FromError(err); ok && s.Code() == codes.Aborted {
			return err
		}
		return status.Errorf(codes.Unavailable, "freeze striped volume %s on node %s: %v", volumeID, node, err)
	}
	if !frozen {
		// Attached and not staged: nothing on the node writes to it either.
		klog.Infof("striped volume %s is attached to %s and not staged there; taking it without a freeze",
			volumeID, node)
		return take(ctx)
	}

	takeCtx, cancel := context.WithDeadline(ctx, start.Add(takeWindow(deadline)))
	takeErr := take(takeCtx)
	cancel()
	held := cs.thaw(ctx, node, volumeID, token)
	frozenFor := time.Since(start)
	klog.Infof("striped volume %s was frozen on node %s for %v (deadline %v, held %v)",
		volumeID, node, frozenFor, deadline, held)

	switch {
	case takeErr != nil:
		return takeErr
	case !held:
		return status.Errorf(codes.Aborted,
			"striped volume %s did not stay frozen on node %s for the %v the members took, so they are not "+
				"one consistent image", volumeID, node, frozenFor)
	}
	return nil
}

// thaw ends a freeze and reports whether it held. A Thaw that cannot be
// delivered reports false: the node resumes at the deadline regardless, and
// nothing taken under an unconfirmed freeze is kept.
func (cs *Server) thaw(ctx context.Context, node, volumeID, token string) bool {
	thawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), thawTimeout)
	defer cancel()
	held, err := cs.freezer.Thaw(thawCtx, node, volumeID, token)
	if err != nil {
		klog.Warningf("thaw striped volume %s on node %s: %v", volumeID, node, err)
		return false
	}
	return held
}

// stagedNodes is the nodes the volume is attached to, from its
// VolumeAttachments, which are where kubelet stages it.
func (cs *Server) stagedNodes(ctx context.Context, volumeID string) ([]string, error) {
	if cs.attachedNodes != nil {
		return cs.attachedNodes(ctx, volumeID)
	}
	if cs.kubeClient == nil {
		return nil, status.Errorf(codes.Unavailable,
			"striped volume %s: without a Kubernetes client the controller cannot tell where it is staged", volumeID)
	}
	pvs, err := cs.kubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list PersistentVolumes: %v", err)
	}
	names := map[string]bool{}
	for i := range pvs.Items {
		if c := pvs.Items[i].Spec.CSI; c != nil && c.VolumeHandle == volumeID {
			names[pvs.Items[i].Name] = true
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	attachments, err := cs.kubeClient.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list VolumeAttachments: %v", err)
	}
	seen := map[string]bool{}
	var nodes []string
	for i := range attachments.Items {
		va := &attachments.Items[i]
		pv := va.Spec.Source.PersistentVolumeName
		// An attachment being torn down still counts until it is gone: the
		// node may not have unstaged yet.
		if pv == nil || !names[*pv] || (!va.Status.Attached && va.DeletionTimestamp == nil) {
			continue
		}
		if !seen[va.Spec.NodeName] {
			seen[va.Spec.NodeName] = true
			nodes = append(nodes, va.Spec.NodeName)
		}
	}
	sort.Strings(nodes)
	return nodes, nil
}
