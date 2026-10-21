// One pass over a storage cluster's metadata server workload that is neither
// the pod template nor the takeover: the state volume, the allowed hosts of
// every pod, the replica count, and the warning for a missing standby
// (design-pnfs-mds-standby.md §4.1, §5.4, P0-2, §10).
//
// It never blocks and never fails the caller for a control-plane refusal. A
// refusal is an event and a requeue, and the work that does not depend on it
// still happens, so one volume that cannot be updated does not hold up the
// promotion of the others.

package mdsstate

import (
	"context"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/lvol"
)

const (
	// retryInterval is how long a refused control-plane call waits.
	retryInterval = 30 * time.Second
	// switchoverInterval is how long a scale-down waits for the active to move.
	switchoverInterval = 10 * time.Second
)

// Event reasons the coordinator emits. MDSStateUnavailable is the metadata
// server's existing reason and now names a failed state volume.
const (
	ReasonStateUnavailable = "MDSStateUnavailable"
	ReasonHostNotAllowed   = "MDSHostNotAllowed"
	ReasonScaleHeld        = "MDSScaleHeld"
	ReasonStandbyMissing   = "MDSStandbyMissing"
)

// Input is what one pass needs to know.
type Input struct {
	StatefulSet *appsv1.StatefulSet
	ClusterID   string
	// Standbys is spec.pnfs.mds.standbys with its default applied.
	Standbys int32
	// Exports are the storage cluster's export volumes, the oldest export
	// first: the first one's pool holds the state volume.
	Exports []lvol.VolumeHandle
	// Pods are the StatefulSet's pods.
	Pods []corev1.Pod
}

// Result is what the pass did, for the caller's own decisions.
type Result struct {
	// RequeueAfter is zero when nothing needs a retry.
	RequeueAfter time.Duration
	// StateVolume is the state volume's handle, empty until it exists.
	StateVolume lvol.VolumeHandle
	// Hosts says which volumes have every pod's host allowed.
	Hosts HostResult
	// Scale is what the scale step did.
	Scale ScaleResult
}

// Events records an event on the driver. It is a function so this package
// needs no recorder of its own.
type Events func(eventType, reason, message string)

// Coordinator runs the pass.
type Coordinator struct {
	Client client.Client
	// APIFor returns the control-plane client of a storage cluster.
	APIFor func(ctx context.Context, clusterID string) (VolumeAPI, error)
	// Switchover is the takeover's hook, nil until it is wired.
	Switchover Switchover
	Events     Events
}

// Reconcile runs the pass in order: the state volume, the hosts of every
// ordinal that will exist, the scale, the hosts of the ordinals a scale-down
// removed, and the standby check.
func (c *Coordinator) Reconcile(ctx context.Context, in Input) (Result, error) {
	var res Result
	sts := in.StatefulSet
	want := 1 + in.Standbys
	current := want
	if sts.Spec.Replicas != nil {
		current = *sts.Spec.Replicas
	}
	retry := func(d time.Duration) {
		if res.RequeueAfter == 0 || d < res.RequeueAfter {
			res.RequeueAfter = d
		}
	}

	var volumes []lvol.VolumeHandle
	var sync *HostSync
	var state *StateVolume
	if api, err := c.api(ctx, in); err != nil {
		c.event(corev1.EventTypeWarning, ReasonStateUnavailable,
			fmt.Sprintf("cannot reach the control plane of storage cluster %s: %v", in.ClusterID, err))
		retry(retryInterval)
	} else if api != nil {
		state = &StateVolume{Client: c.Client, API: api}
		sync = &HostSync{Client: c.Client, API: api}
	}

	if state != nil {
		handle, err := c.stateVolume(ctx, state, in)
		switch {
		case err != nil:
			c.event(corev1.EventTypeWarning, ReasonStateUnavailable,
				fmt.Sprintf("the state volume of %s: %v", sts.Name, err))
			retry(retryInterval)
		case handle != "":
			res.StateVolume = handle
			volumes = append(volumes, handle)
		}
	}
	volumes = append(volumes, in.Exports...)

	if sync != nil && len(volumes) > 0 {
		hosts, err := sync.Sync(ctx, sts, volumes, max(want, current))
		if err != nil {
			return res, err
		}
		res.Hosts = c.reportHosts(hosts, retry)
	}

	scaler := &Scaler{Client: c.Client, Switchover: c.Switchover}
	scaled, err := scaler.Scale(ctx, sts, want)
	switch {
	case err != nil:
		c.event(corev1.EventTypeWarning, ReasonScaleHeld,
			fmt.Sprintf("StatefulSet %s stays at %d replicas: %v", sts.Name, current, err))
		retry(retryInterval)
	case scaled == ScaleWaitingForSwitchover:
		res.Scale = scaled
		retry(switchoverInterval)
	default:
		res.Scale = scaled
		if sync != nil && len(volumes) > 0 && want < current {
			hosts, err := sync.Sync(ctx, sts, volumes, want)
			if err != nil {
				return res, err
			}
			res.Hosts = c.reportHosts(hosts, retry)
		}
	}

	if StandbyMissing(in.Standbys, sts.Annotations[ActiveAnnotation], in.Pods) {
		c.event(corev1.EventTypeWarning, ReasonStandbyMissing, fmt.Sprintf(
			"no standby of %s is running and ready, so exports stay unserved after the loss of the active", sts.Name))
	}
	return res, nil
}

// stateVolume ensures the state volume once an export exists to name its pool,
// or returns the recorded handle when there is none.
func (c *Coordinator) stateVolume(ctx context.Context, state *StateVolume, in Input) (lvol.VolumeHandle, error) {
	sts := in.StatefulSet
	if sts.Annotations[StateVolumeAnnotation] == "" && len(in.Exports) == 0 {
		return "", nil
	}
	poolRef := ""
	if len(in.Exports) > 0 {
		if h, ok := lvol.ParseHandle(in.Exports[0]); ok {
			poolRef = h.PoolRef
		}
	}
	return state.Ensure(ctx, sts, in.ClusterID, poolRef)
}

// reportHosts turns each failed volume into an event and a retry.
func (c *Coordinator) reportHosts(hosts HostResult, retry func(time.Duration)) HostResult {
	failed := make([]lvol.VolumeHandle, 0, len(hosts.Failed))
	for h := range hosts.Failed {
		failed = append(failed, h)
	}
	slices.Sort(failed)
	for _, h := range failed {
		c.event(corev1.EventTypeWarning, ReasonHostNotAllowed,
			fmt.Sprintf("cannot allow the metadata server's hosts on volume %s: %v", h, hosts.Failed[h]))
		retry(retryInterval)
	}
	return hosts
}

// api returns the cluster's client, or nil when no source is wired.
func (c *Coordinator) api(ctx context.Context, in Input) (VolumeAPI, error) {
	if c.APIFor == nil {
		return nil, nil
	}
	return c.APIFor(ctx, in.ClusterID)
}

func (c *Coordinator) event(eventType, reason, message string) {
	if c.Events != nil {
		c.Events(eventType, reason, message)
	}
}

// Finalize releases a deleted StatefulSet's state volume.
func (c *Coordinator) Finalize(ctx context.Context, sts *appsv1.StatefulSet, clusterID string) error {
	api, err := c.api(ctx, Input{ClusterID: clusterID})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}
	if api == nil {
		return fmt.Errorf("%w: no control-plane client is wired", ErrStateUnavailable)
	}
	return (&StateVolume{Client: c.Client, API: api}).Finalize(ctx, sts)
}
